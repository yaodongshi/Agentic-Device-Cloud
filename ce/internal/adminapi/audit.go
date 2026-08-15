package adminapi

// design/80 B-06: audit log query and export (FR-013). Endpoints:
//
//	GET  /v1/admin/audit-logs          (design/33 3.1.16, cursor paging)
//	POST /v1/admin/audit-logs/export   (synchronous CSV, hard row cap)
//
// adc_audit_logs is append-only and partitioned monthly by created_at
// (design/32 3.10). To stay inside the 3-second query SLA (FR-013
// BR-013-01, design/32 3.1.4) the PostgreSQL AuditQueryRepo must prune
// partitions via the time-range filter and ride the idx_audit_tenant_time
// / idx_audit_device_time / idx_audit_status_time indexes; this package
// spells that contract, the in-memory test repo demonstrates the
// semantics.
//
// Export model (V1): synchronous streaming CSV with a hard row cap,
// chosen over the design/33 async task path for these reasons:
//
//  1. V1 has no export-job infrastructure (task table, worker, object
//     storage, polling endpoint); standing one up inside B-06's
//     four-person-day budget would silently degrade every other
//     deliverable.
//  2. The 100k-row cap bounds the worst-case synchronous stream within
//     the 3-second SLA budget when the caller honors the time-range
//     filter (design/32 3.1.4), so the sync path meets BR-013-01.
//  3. Over-limit requests answer 413 with a narrowing hint (design/40:
//     99999 rows sync / 100k rows async boundary), which lets callers
//     self-service by shrinking time_from/time_to instead of blocking
//     on a job.
//
// The async path (task_id + status_url, design/33 3.1.16 note) stays
// the FR-013 contract for over-limit exports and is a follow-up change.
//
// Note on response shape: design/33 3.1.16 shows a trace_id field, but
// adc_audit_logs has no trace_id column in migration 0001 - request_id
// is the stored correlator (design/32 3.10, 6.3), so items carry
// request_id instead.

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/httpx"
)

const (
	defaultAuditLimit    = 50
	maxAuditLimit        = 500
	defaultExportMaxRows = 100000
	maxKeywordLen        = 256
)

// ErrAuditExportLimitExceeded maps to 413 code 14002 (see package
// comment for the sync-export rationale). Note design/33 1.5 maps 14002
// to HTTP 422; B-06 upgrades it to 413 because the cap guards response
// size - transport semantics - not domain validity.
var ErrAuditExportLimitExceeded = errors.New("adminapi: audit export exceeds the synchronous row cap")

// auditStatuses are the adc_audit_logs.status values (migration 0001:
// success / failed / blocked_by_hitl).
var auditStatuses = map[string]bool{
	"success":         true,
	"failed":          true,
	"blocked_by_hitl": true,
}

// auditEventTypes are the adc_audit_logs.event_type values
// (design/32 3.10).
var auditEventTypes = map[string]bool{
	"tool_call":         true,
	"approval_decision": true,
	"admin_op":          true,
	"auth_event":        true,
	"quota_event":       true,
	"device_event":      true,
}

// AuditLog is one adc_audit_logs row (design/32 3.10) in API spelling.
// DeviceCode is resolved via the adc_devices ledger join (the audit
// table stores device_id only).
type AuditLog struct {
	ID                  string
	TenantID            string
	EventType           string
	ActorType           string
	ActorID             string
	ApiKeyID            string
	DeviceID            string
	DeviceCode          string
	ToolName            string
	RiskLevel           *int
	RequestParams       map[string]any
	ResponsePayload     map[string]any
	ResponseTruncated   bool
	ExecutionDurationMS *int
	Status              string
	HitlTicketID        string
	HitlApprover        string
	HitlComment         string
	ExemptReason        string
	RequestID           string
	CreatedAt           time.Time
}

// AuditFilter narrows audit queries (design/33 3.1.16 + keyword). The
// TenantID is always set by the handler (session-derived, SEC-02); the
// PG implementation must push TimeFrom/TimeTo into the partition
// pruning condition.
type AuditFilter struct {
	TenantID   string
	TimeFrom   *time.Time
	TimeTo     *time.Time
	DeviceID   string
	DeviceCode string
	ToolName   string
	Status     string
	EventType  string
	AgentID    string // matches actor_id
	Keyword    string // case-insensitive over tool_name/actor_id/hitl_approver/hitl_comment
}

// AuditCursor is the keyset pagination cursor (design/33 1.6): the
// (created_at, id) composite key of the last row of the previous page,
// rows ordered created_at DESC, id DESC.
type AuditCursor struct {
	CreatedAt time.Time
	ID        string
}

// AuditPage is one keyset window plus the window total (used by the
// export cap pre-check; the list endpoint does not serialize Total,
// design/33 1.6).
type AuditPage struct {
	Items      []AuditLog
	NextCursor *AuditCursor
	Total      int
}

// AuditQueryRepo serves the audit read surface (design/31 LLD 3.4.2).
type AuditQueryRepo interface {
	// Query returns one keyset window after the cursor (nil = first
	// page) sorted created_at DESC, id DESC; NextCursor is nil on the
	// last page.
	Query(ctx context.Context, f AuditFilter, after *AuditCursor, limit int) (*AuditPage, error)
	// Count reports the rows matching the filter (export cap check).
	Count(ctx context.Context, f AuditFilter) (int, error)
	// Export streams matching rows as CSV to w, capped at maxRows;
	// ErrAuditExportLimitExceeded aborts the stream once the cap is
	// passed.
	Export(ctx context.Context, f AuditFilter, maxRows int, w io.Writer) (int, error)
}

// ---------------------------------------------------------------------------
// cursor encoding (design/33 1.6: base64, clients must not construct)
// ---------------------------------------------------------------------------

// auditCursorJSON is the wire form of AuditCursor. Versioned so a
// future cursor shape can be distinguished.
type auditCursorJSON struct {
	V int    `json:"v"`
	C string `json:"c"` // created_at RFC3339Nano UTC
	I string `json:"i"` // log id
}

const auditCursorVersion = 1

func encodeAuditCursor(c *AuditCursor) string {
	if c == nil {
		return ""
	}
	raw, err := json.Marshal(auditCursorJSON{
		V: auditCursorVersion,
		C: c.CreatedAt.UTC().Format(time.RFC3339Nano),
		I: c.ID,
	})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeAuditCursor(s string) (*AuditCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("invalid cursor")
	}
	var p auditCursorJSON
	if err := json.Unmarshal(raw, &p); err != nil || p.V != auditCursorVersion || p.I == "" {
		return nil, errors.New("invalid cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, p.C)
	if err != nil {
		return nil, errors.New("invalid cursor")
	}
	return &AuditCursor{CreatedAt: t, ID: p.I}, nil
}

// ---------------------------------------------------------------------------
// filter parsing
// ---------------------------------------------------------------------------

// auditFilterRequest carries the filter fields as they arrive on the
// GET query string or the export JSON body (design/33 3.1.16 params).
type auditFilterRequest struct {
	TimeFrom   string `json:"time_from"`
	TimeTo     string `json:"time_to"`
	DeviceID   string `json:"device_id"`
	DeviceCode string `json:"device_code"`
	ToolName   string `json:"tool_name"`
	Status     string `json:"status"`
	EventType  string `json:"event_type"`
	AgentID    string `json:"agent_id"`
	Keyword    string `json:"keyword"`
}

func auditFilterFromQuery(q url.Values) auditFilterRequest {
	get := func(k string) string { return strings.TrimSpace(q.Get(k)) }
	return auditFilterRequest{
		TimeFrom:   get("time_from"),
		TimeTo:     get("time_to"),
		DeviceID:   get("device_id"),
		DeviceCode: get("device_code"),
		ToolName:   get("tool_name"),
		Status:     get("status"),
		EventType:  get("event_type"),
		AgentID:    get("agent_id"),
		Keyword:    get("keyword"),
	}
}

// buildAuditFilter validates and assembles the domain filter. All
// validation failures map to 14001 (design/33: audit query condition
// invalid).
func buildAuditFilter(tenantID string, req auditFilterRequest) (*AuditFilter, error) {
	f := &AuditFilter{
		TenantID:   tenantID,
		DeviceID:   req.DeviceID,
		DeviceCode: req.DeviceCode,
		ToolName:   req.ToolName,
		Status:     req.Status,
		EventType:  req.EventType,
		AgentID:    req.AgentID,
		Keyword:    req.Keyword,
	}
	if req.TimeFrom != "" {
		t, err := time.Parse(time.RFC3339, req.TimeFrom)
		if err != nil {
			return nil, errors.New("time_from must be RFC3339")
		}
		f.TimeFrom = &t
	}
	if req.TimeTo != "" {
		t, err := time.Parse(time.RFC3339, req.TimeTo)
		if err != nil {
			return nil, errors.New("time_to must be RFC3339")
		}
		f.TimeTo = &t
	}
	if f.TimeFrom != nil && f.TimeTo != nil && f.TimeFrom.After(*f.TimeTo) {
		return nil, errors.New("time_from must not be after time_to")
	}
	if f.DeviceID != "" && !isValidUUID(f.DeviceID) {
		return nil, errors.New("device_id must be a valid uuid")
	}
	if f.Status != "" && !auditStatuses[f.Status] {
		return nil, errors.New("status must be one of success, failed, blocked_by_hitl")
	}
	if f.EventType != "" && !auditEventTypes[f.EventType] {
		return nil, errors.New("event_type is not a valid audit event type")
	}
	if len(f.Keyword) > maxKeywordLen {
		return nil, errors.New("keyword is too long")
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

// auditLogItem mirrors design/33 3.1.16 (minus trace_id: the audit
// table stores request_id as the correlator, see package comment).
type auditLogItem struct {
	LogID               string         `json:"log_id"`
	EventType           string         `json:"event_type"`
	ActorType           string         `json:"actor_type"`
	ActorID             string         `json:"actor_id,omitempty"`
	KeyID               string         `json:"key_id,omitempty"`
	DeviceID            string         `json:"device_id,omitempty"`
	DeviceCode          string         `json:"device_code,omitempty"`
	ToolName            string         `json:"tool_name,omitempty"`
	RiskLevel           *int           `json:"risk_level,omitempty"`
	RequestParams       map[string]any `json:"request_params,omitempty"`
	ResponsePayload     map[string]any `json:"response_payload,omitempty"`
	ResponseTruncated   bool           `json:"response_truncated"`
	Status              string         `json:"status"`
	HitlTicketID        string         `json:"hitl_ticket_id,omitempty"`
	HitlApprover        string         `json:"hitl_approver,omitempty"`
	HitlComment         string         `json:"hitl_comment,omitempty"`
	ExemptReason        string         `json:"exempt_reason,omitempty"`
	ExecutionDurationMS *int           `json:"execution_duration_ms,omitempty"`
	RequestID           string         `json:"request_id"`
	CreatedAt           time.Time      `json:"created_at"`
}

type auditLogListResponse struct {
	Items      []auditLogItem `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

func newAuditLogItem(l *AuditLog) auditLogItem {
	return auditLogItem{
		LogID:               l.ID,
		EventType:           l.EventType,
		ActorType:           l.ActorType,
		ActorID:             l.ActorID,
		KeyID:               l.ApiKeyID,
		DeviceID:            l.DeviceID,
		DeviceCode:          l.DeviceCode,
		ToolName:            l.ToolName,
		RiskLevel:           l.RiskLevel,
		RequestParams:       l.RequestParams,
		ResponsePayload:     l.ResponsePayload,
		ResponseTruncated:   l.ResponseTruncated,
		Status:              l.Status,
		HitlTicketID:        l.HitlTicketID,
		HitlApprover:        l.HitlApprover,
		HitlComment:         l.HitlComment,
		ExemptReason:        l.ExemptReason,
		ExecutionDurationMS: l.ExecutionDurationMS,
		RequestID:           l.RequestID,
		CreatedAt:           l.CreatedAt.UTC(),
	}
}

// handleAuditLogs serves GET /v1/admin/audit-logs with multi-condition
// filters and keyset cursor pagination (design/33 1.6, 3.1.16).
func (s *Server) handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	if s.AuditQuery == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "audit repository not wired")
		return
	}
	f, err := buildAuditFilter(tenantID, auditFilterFromQuery(r.URL.Query()))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeAuditQueryInvalid, err.Error())
		return
	}
	limit := defaultAuditLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 || n > maxAuditLimit {
			writeError(w, r, http.StatusBadRequest, codeAuditQueryInvalid, "limit must be between 1 and 500")
			return
		}
		limit = n
	}
	var after *AuditCursor
	if v := strings.TrimSpace(r.URL.Query().Get("cursor")); v != "" {
		after, err = decodeAuditCursor(v)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, codeAuditQueryInvalid, err.Error())
			return
		}
	}
	page, err := s.AuditQuery.Query(r.Context(), *f, after, limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "audit query failed")
		return
	}
	items := make([]auditLogItem, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, newAuditLogItem(&page.Items[i]))
	}
	var nextCursor *string
	if page.NextCursor != nil {
		v := encodeAuditCursor(page.NextCursor)
		nextCursor = &v
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: tenantID,
		ActorID:  p.UserID,
		Action:   "audit.query",
		Target:   tenantID,
		Details: map[string]any{
			"row_count": len(items),
			"total":     page.Total,
		},
		TraceID: httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusOK, auditLogListResponse{Items: items, NextCursor: nextCursor})
}

// handleAuditExport serves POST /v1/admin/audit-logs/export: synchronous
// CSV streaming capped at ExportMaxRows (default 100k). The time range
// is mandatory here - bulk egress without a time window would defeat
// partition pruning and the 3-second budget (design/31 3.4.5).
func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	if s.AuditQuery == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "audit repository not wired")
		return
	}
	var req auditFilterRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.TimeFrom == "" || req.TimeTo == "" {
		writeError(w, r, http.StatusBadRequest, codeAuditQueryInvalid, "time_from and time_to are required for export")
		return
	}
	f, err := buildAuditFilter(tenantID, req)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeAuditQueryInvalid, err.Error())
		return
	}
	cap := s.ExportMaxRows
	if cap <= 0 {
		cap = defaultExportMaxRows
	}
	count, err := s.AuditQuery.Count(r.Context(), *f)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "audit count failed")
		return
	}
	if count > cap {
		writeError(w, r, http.StatusRequestEntityTooLarge, codeAuditExportLimit,
			fmt.Sprintf("export matches %d rows, over the %d row synchronous cap; narrow time_from/time_to or use the async export", count, cap))
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="adc-audit-logs.csv"`)
	written, err := s.AuditQuery.Export(r.Context(), *f, cap, w)
	if errors.Is(err, ErrAuditExportLimitExceeded) {
		// Count/Export race: the stream is already partially written and
		// no error body can follow the CSV; the client must narrow the
		// range and retry.
		return
	}
	if err != nil {
		// Stream already started; nothing sensible to write back.
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: tenantID,
		ActorID:  p.UserID,
		Action:   "audit.export",
		Target:   tenantID,
		Details: map[string]any{
			"row_count": written,
			"time_from": req.TimeFrom,
			"time_to":   req.TimeTo,
		},
		TraceID: httpx.TraceIDFrom(r),
	})
}

// ---------------------------------------------------------------------------
// CSV shape (shared with repository implementations)
// ---------------------------------------------------------------------------

var auditCSVHeader = []string{
	"log_id", "tenant_id", "event_type", "actor_type", "actor_id",
	"api_key_id", "device_id", "device_code", "tool_name", "risk_level",
	"status", "hitl_ticket_id", "hitl_approver", "hitl_comment",
	"exempt_reason", "execution_duration_ms", "request_id",
	"response_truncated", "request_params", "response_payload",
	"created_at",
}

// auditCSVRow renders one audit row; JSONB fields are compact-encoded.
func auditCSVRow(l AuditLog) []string {
	risk := ""
	if l.RiskLevel != nil {
		risk = strconv.Itoa(*l.RiskLevel)
	}
	dur := ""
	if l.ExecutionDurationMS != nil {
		dur = strconv.Itoa(*l.ExecutionDurationMS)
	}
	var params, payload []byte
	if l.RequestParams != nil {
		params, _ = json.Marshal(l.RequestParams)
	}
	if l.ResponsePayload != nil {
		payload, _ = json.Marshal(l.ResponsePayload)
	}
	return []string{
		l.ID, l.TenantID, l.EventType, l.ActorType, l.ActorID,
		l.ApiKeyID, l.DeviceID, l.DeviceCode, l.ToolName, risk,
		l.Status, l.HitlTicketID, l.HitlApprover, l.HitlComment,
		l.ExemptReason, dur, l.RequestID,
		strconv.FormatBool(l.ResponseTruncated), string(params), string(payload),
		l.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// writeAuditCSV streams rows through encoding/csv.
func writeAuditCSV(w io.Writer, rows [][]string) error {
	cw := csv.NewWriter(w)
	for _, row := range rows {
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
