package adminapi

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// importHeader is the canonical CSV header accepted by the import
// endpoint (group_ids is optional and omitted in most fixtures).
const importHeader = "device_code,name,device_type,auth_type\n"

// postImport uploads a CSV file through POST /v1/admin/devices/import.
func postImport(t *testing.T, env *testEnv, tok, csvContent string, dryRun bool) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "devices.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(csvContent)); err != nil {
		t.Fatal(err)
	}
	if dryRun {
		if err := mw.WriteField("dry_run", "true"); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/devices/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

// waitImportJob polls GET /v1/admin/devices/import/jobs/{jobID} until the job
// reaches a terminal state; returns the final snapshot.
func waitImportJob(t *testing.T, env *testEnv, tok, jobID string) importStatusResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := env.do(http.MethodGet, "/v1/admin/devices/import/jobs/"+jobID, tok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("import status = %d; body: %s", rec.Code, rec.Body.String())
		}
		var out importStatusResponse
		env.decode(t, rec, &out)
		if out.Status == importJobStatusDone || out.Status == importJobStatusFailed {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("import job %s did not finish; last snapshot: %+v", jobID, out)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startImport posts the CSV and returns the 202 task id (or fails).
func startImport(t *testing.T, env *testEnv, tok, csvContent string, dryRun bool) string {
	t.Helper()
	rec := postImport(t, env, tok, csvContent, dryRun)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("import status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var acc importAcceptedResponse
	env.decode(t, rec, &acc)
	if acc.TaskID == "" || acc.Status != importJobStatusQueued || acc.StatusURL == "" {
		t.Fatalf("202 shape = %+v, want task_id/status=queued/status_url", acc)
	}
	if acc.StatusURL != "/v1/admin/devices/import/jobs/"+acc.TaskID {
		t.Fatalf("status_url = %q", acc.StatusURL)
	}
	return acc.TaskID
}

func deviceListTotal(t *testing.T, env *testEnv, tok string) int {
	t.Helper()
	rec := env.do(http.MethodGet, "/v1/admin/devices", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list devices = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out deviceListResponse
	env.decode(t, rec, &out)
	return out.Total
}

func TestImportDevicesBatchSuccess(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	csv := importHeader +
		"cnc-01,一号车床,cnc,token\n" +
		"agv-01,AGV 小车,agv,hmac\n" +
		"plc-01,PLC 控制器,plc,mtls\n"
	jobID := startImport(t, env, tok, csv, false)
	out := waitImportJob(t, env, tok, jobID)
	if out.Status != importJobStatusDone || out.TotalRows != 3 || out.Success != 3 || out.Failed != 0 {
		t.Fatalf("job = %+v, want done 3/3/0", out)
	}
	if len(out.Errors) != 0 || out.DryRun || out.FinishedAt == nil {
		t.Fatalf("job extras = %+v", out)
	}
	if total := deviceListTotal(t, env, tok); total != 3 {
		t.Fatalf("device total = %d, want 3", total)
	}
	// The audit trail records the import with its counters.
	op, ok := env.audit.last()
	if !ok || op.Action != "device.import" || op.Details["success"] != 3 {
		t.Fatalf("audit = %+v, want device.import with success=3", op)
	}
}

func TestImportDevicesPerRowErrors(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	csv := importHeader +
		"bad code!,车床,cnc,token\n" + // row 2: illegal device_code
		"cnc-ok,一号车床,cnc,token\n" + // row 3: fine
		"agv-bad,,agv,hmac\n" + // row 4: empty name
		"plc-bad,PLC,plc,oauth2_client_credentials\n" // row 5: unsupported auth_type
	jobID := startImport(t, env, tok, csv, false)
	out := waitImportJob(t, env, tok, jobID)
	if out.Success != 1 || out.Failed != 3 || out.Processed != 4 {
		t.Fatalf("counters = %+v, want success=1 failed=3 processed=4", out)
	}
	if len(out.Errors) != 3 {
		t.Fatalf("errors = %+v, want 3 rows", out.Errors)
	}
	wantRows := map[int]string{2: codeBadRequest, 4: codeBadRequest, 5: codeBadRequest}
	for _, e := range out.Errors {
		if wantRows[e.Row] != e.Code {
			t.Fatalf("row %d code = %q, want %q (all: %+v)", e.Row, e.Code, wantRows[e.Row], out.Errors)
		}
		if e.Message == "" {
			t.Fatalf("row %d has no reason message", e.Row)
		}
	}
	// Failed rows are not registered; only the valid row landed.
	if total := deviceListTotal(t, env, tok); total != 1 {
		t.Fatalf("device total = %d, want 1 (failed rows must not register)", total)
	}
}

func TestImportDevicesDuplicateCodeRollsBackRows(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	// Pre-existing device: dup-01 already lives in the ledger.
	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dup-01", "token"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("pre-register: %d; body: %s", rec.Code, rec.Body.String())
	}
	csv := importHeader +
		"dup-01,重复设备,cnc,token\n" + // row 2: duplicate of existing code
		"fresh-01,新设备,cnc,token\n" + // row 3: fine
		"dup-01,重复设备二,cnc,token\n" // row 4: duplicate again
	jobID := startImport(t, env, tok, csv, false)
	out := waitImportJob(t, env, tok, jobID)
	if out.Success != 1 || out.Failed != 2 {
		t.Fatalf("counters = %+v, want success=1 failed=2", out)
	}
	for _, e := range out.Errors {
		if e.Code != codeDeviceCodeExists || e.Row != 2 && e.Row != 4 {
			t.Fatalf("row %d: code=%q, want %q on rows 2/4", e.Row, e.Code, codeDeviceCodeExists)
		}
	}
	// The duplicate rows left no partial devices behind (FR-011: 整行回滚).
	if total := deviceListTotal(t, env, tok); total != 2 {
		t.Fatalf("device total = %d, want 2 (duplicate rows fully rolled back)", total)
	}
}

func TestImportDevicesOver1000RowsRejected(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	var sb strings.Builder
	sb.WriteString(importHeader)
	for i := 1; i <= 1001; i++ {
		fmt.Fprintf(&sb, "dev-%04d,设备%d,cnc,token\n", i, i)
	}
	rec := postImport(t, env, tok, sb.String(), false)
	env.assertError(t, rec, http.StatusRequestEntityTooLarge, codeBodyTooLarge)
	if total := deviceListTotal(t, env, tok); total != 0 {
		t.Fatalf("device total = %d, want 0 (oversized import must not create anything)", total)
	}
}

func TestImportDevicesProgressAndJobNotFound(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	jobID := startImport(t, env, tok, importHeader+"a-01,设备A,cnc,token\nb-01,设备B,cnc,token\n", false)
	// The poll endpoint answers with a progressing snapshot while the
	// worker runs; the status is one of queued/running/done depending on
	// scheduling, and counters are consistent with the snapshot.
	rec := env.do(http.MethodGet, "/v1/admin/devices/import/jobs/"+jobID, tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var snap importStatusResponse
	env.decode(t, rec, &snap)
	switch snap.Status {
	case importJobStatusQueued, importJobStatusRunning, importJobStatusDone:
	default:
		t.Fatalf("snapshot status = %q", snap.Status)
	}
	if snap.TotalRows != 2 || snap.Processed > snap.TotalRows || snap.StatusURL == "" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if out := waitImportJob(t, env, tok, jobID); out.Success != 2 || out.Failed != 0 {
		t.Fatalf("final = %+v, want success=2", out)
	}
	// Unknown and malformed job ids answer 404 (no enumeration hints).
	env.assertError(t, env.do(http.MethodGet, "/v1/admin/devices/import/jobs/imp_ffffffffffffffff", tok, nil),
		http.StatusNotFound, codeNotFound)
	env.assertError(t, env.do(http.MethodGet, "/v1/admin/devices/import/jobs/not-a-job", tok, nil),
		http.StatusNotFound, codeNotFound)
}

func TestImportDevicesDryRunCreatesNothing(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	// First: a clean dry run validates all rows and registers nothing.
	jobID := startImport(t, env, tok, importHeader+
		"a-01,设备A,cnc,token\nb-01,设备B,cnc,token\n", true)
	out := waitImportJob(t, env, tok, jobID)
	if !out.DryRun || out.Success != 2 || out.Failed != 0 {
		t.Fatalf("dry run = %+v, want success=2 failed=0", out)
	}
	if total := deviceListTotal(t, env, tok); total != 0 {
		t.Fatalf("dry run registered %d devices, want 0", total)
	}
	// Second: a dry run still reports duplicate codes.
	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("x-01", "token"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("pre-register: %d; body: %s", rec.Code, rec.Body.String())
	}
	jobID = startImport(t, env, tok, importHeader+"x-01,重复,cnc,token\n", true)
	out = waitImportJob(t, env, tok, jobID)
	if out.Failed != 1 || len(out.Errors) != 1 || out.Errors[0].Code != codeDeviceCodeExists {
		t.Fatalf("dry run duplicate = %+v", out)
	}
}

func TestImportDevicesQuotaExceededPerRow(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", &quotaRequest{MaxDevices: intPtr(2)})
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	jobID := startImport(t, env, tok, importHeader+
		"q-01,设备一,cnc,token\nq-02,设备二,cnc,token\nq-03,设备三,cnc,token\n", false)
	out := waitImportJob(t, env, tok, jobID)
	if out.Success != 2 || out.Failed != 1 {
		t.Fatalf("counters = %+v, want success=2 failed=1 (quota 2)", out)
	}
	if len(out.Errors) != 1 || out.Errors[0].Code != codeDeviceQuota || out.Errors[0].DeviceCode != "q-03" {
		t.Fatalf("quota error = %+v, want q-03 with %q", out.Errors, codeDeviceQuota)
	}
	if total := deviceListTotal(t, env, tok); total != 2 {
		t.Fatalf("device total = %d, want 2", total)
	}
}

func TestImportDevicesBadUploads(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	// JSON body instead of multipart.
	rec := env.do(http.MethodPost, "/v1/admin/devices/import", tok,
		map[string]any{"device_code": "x"})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)

	// Header-only CSV has no data rows.
	env.assertError(t, postImport(t, env, tok, importHeader, false),
		http.StatusBadRequest, codeBadRequest)

	// Missing required header columns.
	env.assertError(t, postImport(t, env, tok, "device_code,name\nx,设备\n", false),
		http.StatusBadRequest, codeBadRequest)

	// Multipart without the file field.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("dry_run", "true"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/devices/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	rec2 := httptest.NewRecorder()
	env.handler.ServeHTTP(rec2, req)
	env.assertError(t, rec2, http.StatusBadRequest, codeBadRequest)
}

func TestImportDevicesAuthAndRoles(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	csv := importHeader + "a-01,设备A,cnc,token\n"

	// Unauthenticated: 401.
	env.assertError(t, postImport(t, env, "", csv, false), http.StatusUnauthorized, codeUnauthorized)

	// Approver: no import access (admin-only route).
	app := env.token(t, []string{"approver"}, tr.TenantID)
	env.assertError(t, postImport(t, env, app, csv, false), http.StatusForbidden, codeForbidden)
	env.assertError(t, env.do(http.MethodGet, "/v1/admin/devices/import/jobs/imp_ffffffffffffffff", app, nil),
		http.StatusForbidden, codeForbidden)

	// Auditor: no import access.
	aud := env.token(t, []string{"auditor"}, tr.TenantID)
	env.assertError(t, postImport(t, env, aud, csv, false), http.StatusForbidden, codeForbidden)

	// Cross-tenant: tenant B cannot read tenant A's job.
	a := env.createTenant(t, "acme-a", "Acme A", nil)
	b := env.createTenant(t, "acme-b", "Acme B", nil)
	aTok := env.token(t, []string{"tenant_admin"}, a.TenantID)
	bTok := env.token(t, []string{"tenant_admin"}, b.TenantID)
	jobID := startImport(t, env, aTok, csv, false)
	waitImportJob(t, env, aTok, jobID)
	env.assertError(t, env.do(http.MethodGet, "/v1/admin/devices/import/jobs/"+jobID, bTok, nil),
		http.StatusForbidden, codeCrossTenant)
}

func TestImportDevicesGroupColumnAndValidation(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	// group_ids column is accepted; an invalid uuid is a per-row error.
	jobID := startImport(t, env, tok,
		"device_code,name,device_type,auth_type,group_ids\n"+
			"g-01,设备一,cnc,token,not-a-uuid\n"+
			"g-02,设备二,cnc,token,\n", false)
	out := waitImportJob(t, env, tok, jobID)
	if out.Success != 1 || out.Failed != 1 {
		t.Fatalf("counters = %+v, want success=1 failed=1", out)
	}
	if out.Errors[0].Row != 2 || out.Errors[0].Code != codeBadRequest {
		t.Fatalf("errors = %+v", out.Errors)
	}
	// A valid group_id attaches the device at registration.
	rec := env.do(http.MethodPost, "/v1/admin/device-groups", tok, map[string]any{"name": "产线A"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create group: %d; body: %s", rec.Code, rec.Body.String())
	}
	var grp groupResponse
	env.decode(t, rec, &grp)
	jobID = startImport(t, env, tok,
		"device_code,name,device_type,auth_type,group_ids\n"+
			"g-03,设备三,cnc,token,"+grp.GroupID+"\n", false)
	out = waitImportJob(t, env, tok, jobID)
	if out.Success != 1 || out.Failed != 0 {
		t.Fatalf("group attach counters = %+v", out)
	}
	env.store.devices.mu.Lock()
	rec3 := env.store.devices.byCode["g-03"]
	attached := env.store.devices.devices[rec3].d.GroupID
	env.store.devices.mu.Unlock()
	if attached == nil || *attached != grp.GroupID {
		t.Fatalf("g-03 group = %v, want %s", attached, grp.GroupID)
	}
}
