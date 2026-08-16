package adminapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/auth"
	"adc.dev/ce/internal/httpx"
)

// Batch device onboarding (FR-011, design/82 B1.1): CSV upload, async
// processing, per-row error report, 1000-row cap.
//
// Transport choice: multipart CSV instead of a JSON array because
// design/33 3.1.9 defines multipart/form-data, factory admins author
// spreadsheets, and the template download pairs naturally with the same
// file shape.
//
// Job store choice: Valkey instead of an adc_import_jobs PG table.
// The PG table is the durable option but needs a schema migration, which
// is out of scope for this change set; an in-memory map loses jobs on
// restart and multi-node deploys (design/60). Valkey (ADR-13) is already
// in the stack, needs no migration, and survives restarts: job state is
// one JSON blob under adc:import_job:{id} with a 24h TTL. When the V1.5
// batch-credential-download follow-up lands, both features can migrate
// onto a shared PG task table (design/31 TaskStore seam).

var ErrImportJobNotFound = errors.New("adminapi: import job not found")

// ErrImportTooManyRows rejects files with more than importMaxRows data
// rows (FR-011 exception path, mapped to 413 code 10008).
var ErrImportTooManyRows = errors.New("adminapi: import exceeds 1000 data rows")

const (
	// importMaxRows is the FR-011 single-file cap (1000 data rows).
	importMaxRows = 1000
	// importMaxBytes caps the multipart body at 2 MiB: 1000 rows of the
	// four required columns stay far below this, while MaxBytesReader
	// guarantees the parse terminates (SEC-19 defense in depth).
	importMaxBytes = 2 << 20
	// importJobTTL keeps job results queryable for 24h (matches the
	// design/33 1.7 idempotency window).
	importJobTTL    = 24 * time.Hour
	importJobPrefix = "adc:import_job:"
)

// Import job statuses (queued -> running -> done | failed).
const (
	importJobStatusQueued  = "queued"
	importJobStatusRunning = "running"
	importJobStatusDone    = "done"
	importJobStatusFailed  = "failed"
)

// importJobIDRe matches the generated job ids ("imp_" + 16 hex chars).
var importJobIDRe = regexp.MustCompile(`^imp_[0-9a-f]{16}$`)

// importCSVColumns are the accepted CSV columns (design/33 3.1.9):
// device_code, name, device_type, auth_type are required; group_ids is
// optional (comma-separated uuids, first entry wins per the single
// adc_devices.group_id column of design/32 3.5).
var importCSVColumns = []struct {
	name     string
	required bool
}{
	{"device_code", true},
	{"name", true},
	{"device_type", true},
	{"auth_type", true},
	{"group_ids", false},
}

// ImportRowError is one failed CSV row (FR-011 acceptance: per-row
// reasons). Code carries the design/33 business code; the transport
// semantics of the same failure on a synchronous request are 400 (10001
// field errors), 409 (11008 duplicate device code) and quota rejection
// (11010).
type ImportRowError struct {
	Row        int    `json:"row"` // CSV line number, header = 1
	DeviceCode string `json:"device_code"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

// ImportJob is the async import task state. TenantID/ActorID never leave
// the server (json:"-"): they exist for ownership checks and the audit
// trail.
type ImportJob struct {
	TaskID    string           `json:"task_id"`
	TenantID  string           `json:"-"`
	ActorID   string           `json:"-"`
	Status    string           `json:"status"`
	DryRun    bool             `json:"dry_run"`
	TotalRows int              `json:"total_rows"`
	Processed int              `json:"processed"`
	Success   int              `json:"success"`
	Failed    int              `json:"failed"`
	Errors    []ImportRowError `json:"errors"`
	// ErrorMsg is set only when the whole job fails (status=failed),
	// e.g. an internal error the worker could not attribute to a row.
	ErrorMsg   string     `json:"error_msg,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// ImportJobRepo persists import job state (design/31 store seam). The
// worker is the only writer after Create; Update stores the full
// snapshot. Implementations: valkeyImportJobRepo (production, restart
// and multi-node safe) and memImportJobRepo (test scaffolding).
type ImportJobRepo interface {
	Create(ctx context.Context, j *ImportJob) error
	Update(ctx context.Context, j *ImportJob) error
	Get(ctx context.Context, jobID string) (*ImportJob, error)
}

// csvImportRow is one parsed data row of the upload.
type csvImportRow struct {
	line                   int // CSV line number, header = 1
	code, name, deviceType string
	authType, groupID      string
}

// newImportJobID returns a random "imp_<16 hex>" id (crypto/rand, same
// discipline as newEventID).
func newImportJobID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "imp_" + hex.EncodeToString([]byte(time.Now().Format("150405.000000000")))
	}
	return "imp_" + hex.EncodeToString(b[:])
}

// parseImportCSV validates the header and returns the data rows. Fatal
// (whole-file) errors: missing header columns, a non-CSV body and
// ErrImportTooManyRows. Per-row field problems are NOT decided here —
// the worker validates rows so failures land in the job's error report
// (FR-011 acceptance), not in the upload response.
func parseImportCSV(r io.Reader) ([]csvImportRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // variable: group_ids column is optional
	header, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty csv file")
		}
		return nil, errors.New("csv parse error: " + err.Error())
	}
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\uFEFF") // Excel UTF-8 BOM
	}
	idx := make([]int, len(importCSVColumns))
	for i := range idx {
		idx[i] = -1
	}
	for i, h := range header {
		h = strings.ToLower(strings.TrimSpace(h))
		for c, col := range importCSVColumns {
			if h == col.name {
				idx[c] = i
			}
		}
	}
	for c, col := range importCSVColumns {
		if col.required && idx[c] < 0 {
			return nil, errors.New("csv header must contain columns: device_code,name,device_type,auth_type (group_ids optional)")
		}
	}

	var rows []csvImportRow
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("csv parse error: " + err.Error())
		}
		if len(rows) >= importMaxRows {
			return nil, ErrImportTooManyRows
		}
		// Column-count mismatches stay per-row so the report tells the
		// admin which line to fix; rows shorter than the required columns
		// fail field validation downstream with an empty-field message,
		// rows longer than the declared columns ignore the trailing cells.
		row := csvImportRow{line: line}
		for c, col := range importCSVColumns {
			if idx[c] < 0 || idx[c] >= len(rec) {
				continue
			}
			v := strings.TrimSpace(rec[idx[c]])
			switch col.name {
			case "device_code":
				row.code = v
			case "name":
				row.name = v
			case "device_type":
				row.deviceType = v
			case "auth_type":
				row.authType = strings.ToLower(v)
			case "group_ids":
				if v != "" {
					row.groupID = strings.Split(v, ",")[0]
					row.groupID = strings.TrimSpace(row.groupID)
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// importAcceptedResponse mirrors design/33 3.1.9 (202 shape).
type importAcceptedResponse struct {
	TaskID    string `json:"task_id"`
	Status    string `json:"status"`
	StatusURL string `json:"status_url"`
}

// importStatusResponse is the GET status body: the job snapshot plus the
// polling URL (design/82 B1.1 progress endpoint).
type importStatusResponse struct {
	ImportJob
	StatusURL string `json:"status_url"`
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

// handleImportDevices serves POST /v1/admin/devices/import (FR-011):
// accepts a multipart CSV upload (field "file", optional "dry_run"),
// enforces the 1000-row cap, creates the async job and answers 202 with
// the polling URL. Row-level outcomes live in the job result, not here.
func (s *Server) handleImportDevices(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	if s.ImportJobs == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "import jobs not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, importMaxBytes)
	// readImportMultipart writes its own error responses, including 413
	// for over-limit bodies (MaxBytesError surfaces from the part reads).
	file, dryRun, ok := readImportMultipart(w, r)
	if !ok {
		return
	}
	rows, err := parseImportCSV(file)
	if err != nil {
		if errors.Is(err, ErrImportTooManyRows) {
			writeError(w, r, http.StatusRequestEntityTooLarge, codeBodyTooLarge,
				"import file exceeds 1000 data rows")
			return
		}
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if len(rows) == 0 {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "csv contains no data rows")
		return
	}
	job := &ImportJob{
		TaskID:    newImportJobID(),
		TenantID:  tenantID,
		ActorID:   p.UserID,
		Status:    importJobStatusQueued,
		DryRun:    dryRun,
		TotalRows: len(rows),
		Errors:    []ImportRowError{},
		CreatedAt: s.now().UTC(),
	}
	if err := s.ImportJobs.Create(r.Context(), job); err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "create import job failed")
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: tenantID,
		ActorID:  p.UserID,
		Action:   "device.import",
		Target:   job.TaskID,
		Details:  map[string]any{"rows": job.TotalRows, "dry_run": job.DryRun},
		TraceID:  httpx.TraceIDFrom(r),
	})
	go s.runImportJob(job, rows)
	httpx.WriteJSON(w, http.StatusAccepted, importAcceptedResponse{
		TaskID:    job.TaskID,
		Status:    importJobStatusQueued,
		StatusURL: "/v1/admin/devices/import/jobs/" + job.TaskID,
	})
}

// readImportMultipart reads the multipart body: "file" (required, one
// occurrence) and "dry_run" (optional boolean). Returns ok=false after
// writing an error response, including 413 for over-limit bodies. The
// file part is buffered immediately: multipart.Reader.NextPart drains
// the previous part when advancing, so a lazily held part would arrive
// empty at the CSV parser. Memory is bounded by importMaxBytes.
func readImportMultipart(w http.ResponseWriter, r *http.Request) (io.Reader, bool, bool) {
	mr, err := r.MultipartReader()
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, r, http.StatusRequestEntityTooLarge, codeBodyTooLarge, "request body too large")
			return nil, false, false
		}
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "multipart/form-data request required")
		return nil, false, false
	}
	var (
		fileBytes []byte
		found     bool
		dryRun    bool
	)
	partErr := func(err error) (io.Reader, bool, bool) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, r, http.StatusRequestEntityTooLarge, codeBodyTooLarge, "request body too large")
		} else {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "malformed multipart body")
		}
		return nil, false, false
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return partErr(err)
		}
		switch part.FormName() {
		case "file":
			if found {
				writeError(w, r, http.StatusBadRequest, codeBadRequest, "duplicate file field")
				return nil, false, false
			}
			fileBytes, err = io.ReadAll(part)
			if err != nil {
				return partErr(err)
			}
			found = true
		case "dry_run":
			raw, _ := io.ReadAll(io.LimitReader(part, 64))
			switch strings.ToLower(strings.TrimSpace(string(raw))) {
			case "true", "1":
				dryRun = true
			}
		default:
			// Unknown fields are ignored (design/33 1.1 tolerance); their
			// content must still be drained so the parser reaches the
			// terminating boundary.
			_, _ = io.Copy(io.Discard, part)
		}
	}
	if !found {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "file field is required")
		return nil, false, false
	}
	return bytes.NewReader(fileBytes), dryRun, true
}

// runImportJob executes the queued job in a goroutine detached from the
// request context: the upload response returns immediately (FR-011 async
// acceptance) and the job must survive it. Work is bounded (<= 1000 rows,
// per-repo timeouts) so no explicit deadline is needed.
func (s *Server) runImportJob(job *ImportJob, rows []csvImportRow) {
	ctx := context.Background()
	job.Status = importJobStatusRunning
	job.StartedAt = s.now().UTC()
	_ = s.ImportJobs.Update(ctx, job)

	for i := range rows {
		row := rows[i]
		job.Processed++
		code, msg := s.importRow(ctx, job, row)
		if code == "" {
			job.Success++
		} else {
			job.Failed++
			job.Errors = append(job.Errors, ImportRowError{
				Row:        row.line,
				DeviceCode: row.code,
				Code:       code,
				Message:    msg,
			})
		}
		// Persist after every row: the progress endpoint shows partial
		// results while the job is still running (FR-011 progress view).
		_ = s.ImportJobs.Update(ctx, job)
	}
	finished := s.now().UTC()
	job.Status = importJobStatusDone
	job.FinishedAt = &finished
	_ = s.ImportJobs.Update(ctx, job)

	s.recordAudit(ctx, AdminOp{
		EventID:  newEventID(),
		TenantID: job.TenantID,
		ActorID:  job.ActorID,
		Action:   "device.import",
		Target:   job.TaskID,
		Details: map[string]any{
			"rows": job.TotalRows, "success": job.Success, "failed": job.Failed,
			"dry_run": job.DryRun,
		},
	})
}

// importRow validates and registers one CSV row through the same rules
// and repository path as handleRegisterDevice (FR-011: reuse the
// registration path). Returns ("", "") on success and (code, message) on
// failure. Failed rows are fully rolled back: Register is per-row
// transactional (insert + quota consumption commit together, design/32
// 6.2), so a duplicate code or quota rejection leaves no partial device.
func (s *Server) importRow(ctx context.Context, job *ImportJob, row csvImportRow) (code, msg string) {
	if !deviceCodeRe.MatchString(row.code) {
		return codeBadRequest, "device_code must be 1-128 chars of letters, digits, dash or underscore"
	}
	if row.name == "" || len(row.name) > 255 {
		return codeBadRequest, "name is required (max 255 chars)"
	}
	if row.deviceType == "" || len(row.deviceType) > 64 {
		return codeBadRequest, "device_type is required (max 64 chars)"
	}
	switch row.authType {
	case auth.AuthTypeToken, auth.AuthTypeHMAC, auth.AuthTypeMTLS:
	default:
		return codeBadRequest, "auth_type must be one of token, hmac, mtls"
	}
	var groupID *string
	if row.groupID != "" {
		if !isValidUUID(row.groupID) {
			return codeBadRequest, "group_ids must be a comma-separated list of uuids"
		}
		gid := row.groupID
		groupID = &gid
	}
	if job.DryRun {
		// Dry run probes uniqueness only; quota consumption is a
		// write-side effect with no cheap read-only equivalent, so a
		// dry run can still overstate quota headroom.
		if _, err := s.Devices.GetByCode(ctx, row.code); err == nil {
			return codeDeviceCodeExists, "device code already exists"
		} else if !errors.Is(err, ErrDeviceNotFound) {
			return codeInternal, "duplicate check failed"
		}
		return "", ""
	}
	plaintext, stored, err := issueCredential(row.authType, s.KEK)
	if err != nil {
		return codeInternal, "credential issuance failed"
	}
	_ = plaintext
	// Batch issuance intentionally discards credentials (NFR-004: the
	// plaintext exists only in issuance responses). Imported devices go
	// through the per-device reset flow or the V1.5 batch credential
	// download before first use (FR-011 note in design/33 3.1.9).
	now := s.now().UTC()
	_, err = s.Devices.Register(ctx, &Device{
		TenantID:   job.TenantID,
		GroupID:    groupID,
		DeviceCode: row.code,
		Name:       row.name,
		DeviceType: row.deviceType,
		AuthType:   row.authType,
		Status:     deviceStatusOffline,
		Metadata:   map[string]any{},
		CreatedAt:  now,
		UpdatedAt:  now,
	}, DeviceCredential{Stored: stored})
	if err != nil {
		switch {
		case errors.Is(err, ErrDeviceCodeExists):
			return codeDeviceCodeExists, "device code already exists"
		case errors.Is(err, ErrDeviceQuotaExceeded):
			return codeDeviceQuota, "device quota exceeded"
		case errors.Is(err, ErrTenantNotFound):
			return codeTenantNotFound, "tenant not found"
		case errors.Is(err, ErrTenantSuspended):
			return codeTenantSuspended, "tenant suspended"
		default:
			return codeInternal, "register device failed"
		}
	}
	return "", ""
}

// handleImportStatus serves GET /v1/admin/devices/import/jobs/{jobID}: the
// progress poll endpoint (FR-011 acceptance, design/82 B1.1). Jobs are
// tenant-scoped; platform admins pass the ownership check by role.
func (s *Server) handleImportStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	jobID := r.PathValue("jobID")
	if !importJobIDRe.MatchString(jobID) {
		writeError(w, r, http.StatusNotFound, codeNotFound, "import job not found")
		return
	}
	if s.ImportJobs == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "import jobs not configured")
		return
	}
	job, err := s.ImportJobs.Get(r.Context(), jobID)
	if errors.Is(err, ErrImportJobNotFound) {
		writeError(w, r, http.StatusNotFound, codeNotFound, "import job not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load import job failed")
		return
	}
	if !s.checkOwnership(w, r, p, job.TenantID) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, importStatusResponse{
		ImportJob: *job,
		StatusURL: "/v1/admin/devices/import/jobs/" + job.TaskID,
	})
}

// ---------------------------------------------------------------------------
// Valkey repository
// ---------------------------------------------------------------------------

// valkeyImportJobRepo stores each job as one JSON snapshot under
// adc:import_job:{id} with a 24h TTL (design/31 store seam). Snapshot
// writes are atomic key SETs; the worker is the only writer after Create
// so no read-modify-write races exist.
type valkeyImportJobRepo struct {
	rdb *redis.Client
}

// NewValkeyImportJobRepo builds the import job repo over a Valkey client.
func NewValkeyImportJobRepo(rdb *redis.Client) *valkeyImportJobRepo {
	return &valkeyImportJobRepo{rdb: rdb}
}

func (r *valkeyImportJobRepo) Create(ctx context.Context, j *ImportJob) error {
	return r.store(ctx, j)
}

func (r *valkeyImportJobRepo) Update(ctx context.Context, j *ImportJob) error {
	return r.store(ctx, j)
}

func (r *valkeyImportJobRepo) store(ctx context.Context, j *ImportJob) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return r.rdb.Set(ctx, importJobPrefix+j.TaskID, raw, importJobTTL).Err()
}

func (r *valkeyImportJobRepo) Get(ctx context.Context, jobID string) (*ImportJob, error) {
	raw, err := r.rdb.Get(ctx, importJobPrefix+jobID).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrImportJobNotFound
	}
	if err != nil {
		return nil, err
	}
	var j ImportJob
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, err
	}
	return &j, nil
}
