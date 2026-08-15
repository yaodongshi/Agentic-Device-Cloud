// Contract tests for the device ledger endpoints (design/33 3.1.6-3.1.8,
// FR-010): one-shot credential issuance, KEK-encrypted at-rest storage
// (NFR-004), code uniqueness, freeze/revoke/reset lifecycle and the
// cross-tenant ownership gate (13007, SEC-02).
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"adc.dev/ce/internal/auth"
)

// registerDevice drives POST /v1/admin/devices for the given admin token
// and target tenant; returns the recorder plus the decoded response.
func (e *testEnv) registerDevice(t *testing.T, token, tenantID, code, name string) (int, []byte) {
	t.Helper()
	rec := e.do(http.MethodPost, "/v1/admin/devices?tenant_id="+tenantID, map[string]any{
		"device_code": code,
		"name":        name,
		"device_type": "cnc",
		"auth_type":   "hmac",
		"metadata":    map[string]any{"workshop": "A1"},
	}, authHdr(token))
	return rec.Code, rec.Body.Bytes()
}

// decodeDeviceRegister parses a 201 device registration response.
func decodeDeviceRegister(t *testing.T, raw []byte) (deviceID, secret string) {
	t.Helper()
	var resp struct {
		DeviceID   string `json:"device_id"`
		DeviceCode string `json:"device_code"`
		Status     string `json:"status"`
		Credential *struct {
			Secret string `json:"secret"`
		} `json:"credential"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode device response: %v (body=%q)", err, raw)
	}
	if resp.DeviceID == "" || resp.Status != "offline" {
		t.Fatalf("device response = %+v", resp)
	}
	if resp.Credential == nil || resp.Credential.Secret == "" {
		t.Fatal("credential.secret missing from one-shot issuance response")
	}
	return resp.DeviceID, resp.Credential.Secret
}

// TestRegisterDeviceOneShotSecret covers design/33 3.1.6 end to end: the
// secret appears exactly once in the 201 response; the ledger stores the
// KEK-encrypted form ("encv1$", decryptable back to the plaintext) and
// never the plaintext (NFR-004); the list endpoint leaks no credential.
func TestRegisterDeviceOneShotSecret(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)
	code := testTenantCode(t, "it-dev")

	status, raw := e.registerDevice(t, token, tenantID, code, "One Shot Lathe")
	if status != http.StatusCreated {
		t.Fatalf("register = %d (body=%q)", status, raw)
	}
	deviceID, secret := decodeDeviceRegister(t, raw)
	if secret == "" {
		t.Fatal("empty secret issued")
	}

	// At-rest assertion: credential_hash is the KEK-encrypted form and
	// decrypts to exactly the issued plaintext (auth.DecryptSecret).
	var stored string
	err := e.pool.QueryRow(context.Background(),
		`SELECT credential_hash FROM adc_devices WHERE id = $1::uuid`, deviceID).Scan(&stored)
	if err != nil {
		t.Fatalf("load credential_hash: %v", err)
	}
	if !strings.HasPrefix(stored, "encv1$") {
		t.Fatalf("credential_hash = %q, want encv1$ KEK-encrypted form", stored)
	}
	if strings.Contains(stored, secret) {
		t.Fatal("plaintext secret leaked into the ledger")
	}
	decrypted, err := auth.DecryptSecret(stored, e.kek)
	if err != nil {
		t.Fatalf("decrypt stored credential: %v", err)
	}
	if decrypted != secret {
		t.Fatal("stored credential does not round-trip to the issued secret")
	}

	// List endpoint: the device appears, but no credential field exists
	// (design/33 3.1.7 note).
	rec := e.do(http.MethodGet, "/v1/admin/devices?tenant_id="+tenantID, nil, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), code) {
		t.Fatalf("device %s missing from list", code)
	}
	if strings.Contains(rec.Body.String(), "credential") || strings.Contains(rec.Body.String(), secret) {
		t.Fatal("device list leaks credential fields")
	}
}

// TestRegisterDeviceDuplicateCode: a second registration with the same
// device_code answers 409 code 11008 (global uniqueness, design/33 3.1.6).
func TestRegisterDeviceDuplicateCode(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)
	code := testTenantCode(t, "it-dup")

	if status, raw := e.registerDevice(t, token, tenantID, code, "First"); status != http.StatusCreated {
		t.Fatalf("first register = %d (body=%q)", status, raw)
	}
	rec := e.do(http.MethodPost, "/v1/admin/devices?tenant_id="+tenantID, map[string]any{
		"device_code": code,
		"name":        "Second",
		"device_type": "cnc",
		"auth_type":   "hmac",
	}, authHdr(token))
	wantErr(t, rec, http.StatusConflict, "11008")
}

// TestRegisterDeviceValidation: SEC-20 charset and enum checks answer 400
// code 10001 (design/33 3.1.6).
func TestRegisterDeviceValidation(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"empty code", map[string]any{"device_code": "", "name": "X", "device_type": "cnc", "auth_type": "hmac"}},
		{"code with space", map[string]any{"device_code": "bad code", "name": "X", "device_type": "cnc", "auth_type": "hmac"}},
		{"code with dot", map[string]any{"device_code": "bad.code", "name": "X", "device_type": "cnc", "auth_type": "hmac"}},
		{"missing name", map[string]any{"device_code": testTenantCode(t, "it-val"), "device_type": "cnc", "auth_type": "hmac"}},
		{"bad auth_type", map[string]any{"device_code": testTenantCode(t, "it-val"), "name": "X", "device_type": "cnc", "auth_type": "open_sesame"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := e.do(http.MethodPost, "/v1/admin/devices?tenant_id="+tenantID, c.body, authHdr(token))
			wantErr(t, rec, http.StatusBadRequest, "10001")
		})
	}
}

// TestDeviceFreezeUnfreezeResetRevoke walks the full lifecycle of
// design/33 3.1.8: freeze -> frozen, unfreeze -> offline, reset_credential
// -> fresh secret (old one replaced), revoke -> tombstoned credential.
func TestDeviceFreezeUnfreezeResetRevoke(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)
	code := testTenantCode(t, "it-life")

	status, raw := e.registerDevice(t, token, tenantID, code, "Lifecycle")
	if status != http.StatusCreated {
		t.Fatalf("register = %d (body=%q)", status, raw)
	}
	deviceID, firstSecret := decodeDeviceRegister(t, raw)

	// Freeze (change_reason mandatory).
	rec := e.do(http.MethodPatch, "/v1/admin/devices/"+deviceID, map[string]any{
		"op": "freeze", "change_reason": "maintenance",
	}, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("freeze = %d (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"frozen"`) {
		t.Fatalf("freeze response status = %q", rec.Body.String())
	}

	// reset_credential on a frozen device -> 403 code 11003.
	rec = e.do(http.MethodPatch, "/v1/admin/devices/"+deviceID, map[string]any{
		"op": "reset_credential", "change_reason": "attempt",
	}, authHdr(token))
	wantErr(t, rec, http.StatusForbidden, "11003")

	// Unfreeze -> offline.
	rec = e.do(http.MethodPatch, "/v1/admin/devices/"+deviceID, map[string]any{
		"op": "unfreeze", "change_reason": "maintenance done",
	}, authHdr(token))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"offline"`) {
		t.Fatalf("unfreeze = %d (body=%q)", rec.Code, rec.Body.String())
	}

	// Reset -> new secret returned, ledger replaced, version bumped.
	rec = e.do(http.MethodPatch, "/v1/admin/devices/"+deviceID, map[string]any{
		"op": "reset_credential", "change_reason": "rotation",
	}, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var resetResp struct {
		Credential *struct {
			Secret string `json:"secret"`
		} `json:"credential"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resetResp)
	if resetResp.Credential == nil || resetResp.Credential.Secret == "" ||
		resetResp.Credential.Secret == firstSecret {
		t.Fatalf("reset credential = %+v", resetResp.Credential)
	}
	var version int
	var stored string
	err := e.pool.QueryRow(context.Background(),
		`SELECT credential_version, credential_hash FROM adc_devices WHERE id = $1::uuid`, deviceID).
		Scan(&version, &stored)
	if err != nil || version != 2 {
		t.Fatalf("ledger after reset: version=%d err=%v", version, err)
	}
	if dec, err := auth.DecryptSecret(stored, e.kek); err != nil || dec != resetResp.Credential.Secret {
		t.Fatal("ledger does not hold the newly issued secret")
	}

	// Revoke -> credential tombstoned (can never verify again).
	rec = e.do(http.MethodPatch, "/v1/admin/devices/"+deviceID, map[string]any{
		"op": "revoke_credential", "change_reason": "decommissioning",
	}, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d (body=%q)", rec.Code, rec.Body.String())
	}
	err = e.pool.QueryRow(context.Background(),
		`SELECT credential_hash FROM adc_devices WHERE id = $1::uuid`, deviceID).Scan(&stored)
	if err != nil {
		t.Fatalf("load revoked credential: %v", err)
	}
	if !strings.HasPrefix(stored, "revoked$") {
		t.Fatalf("revoked credential_hash = %q, want revoked$ tombstone", stored)
	}
}

// TestDeviceListFiltersAndPagination: GET /v1/admin/devices honors the
// status filter and offset pagination (design/33 3.1.7, 1.6).
func TestDeviceListFiltersAndPagination(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)
	code := testTenantCode(t, "it-list")

	status, raw := e.registerDevice(t, token, tenantID, code, "Filter Target")
	if status != http.StatusCreated {
		t.Fatalf("register = %d (body=%q)", status, raw)
	}
	deviceID, _ := decodeDeviceRegister(t, raw)

	// Freeze it so the status filter has a distinct value to hit.
	rec := e.do(http.MethodPatch, "/v1/admin/devices/"+deviceID, map[string]any{
		"op": "freeze", "change_reason": "filter test",
	}, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("freeze = %d", rec.Code)
	}

	rec = e.do(http.MethodGet, "/v1/admin/devices?tenant_id="+tenantID+"&status=frozen&page=1&page_size=1", nil, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered list = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []struct {
			DeviceID string `json:"device_id"`
			Status   string `json:"status"`
		} `json:"items"`
		Total    int `json:"total"`
		Page     int `json:"page"`
		PageSize int `json:"page_size"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.PageSize != 1 || len(list.Items) != 1 {
		t.Fatalf("pagination = page_size %d items %d", list.PageSize, len(list.Items))
	}
	if list.Items[0].Status != "frozen" || list.Items[0].DeviceID != deviceID {
		t.Fatalf("filtered item = %+v", list.Items[0])
	}

	// Invalid status filter -> 400 code 10001.
	rec = e.do(http.MethodGet, "/v1/admin/devices?tenant_id="+tenantID+"&status=ghost", nil, authHdr(token))
	wantErr(t, rec, http.StatusBadRequest, "10001")
}

// TestDeviceCrossTenantForbidden: a tenant_admin touching another tenant's
// device gets 403 code 13007 without learning whether it exists (SEC-02,
// design/33 3.1.8; TEN-003).
func TestDeviceCrossTenantForbidden(t *testing.T) {
	e := envOrSkip(t)
	platform := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	code := testTenantCode(t, "it-x")
	status, raw := e.registerDevice(t, platform, e.tenantID(t, fixtureBetaTenant), code, "Beta Asset")
	if status != http.StatusCreated {
		t.Fatalf("register = %d (body=%q)", status, raw)
	}
	deviceID, _ := decodeDeviceRegister(t, raw)

	alpha := e.loginOK(t, fixtureAlphaAdmin, fixtureAlphaPass)
	rec := e.do(http.MethodPatch, "/v1/admin/devices/"+deviceID, map[string]any{
		"op": "freeze", "change_reason": "cross-tenant attempt",
	}, authHdr(alpha))
	wantErr(t, rec, http.StatusForbidden, "13007")
}

// TestDeviceQuotaExceeded: with a max_devices=1 quota the second
// registration answers 403 code 11010 (design/33 3.1.6 quota error).
func TestDeviceQuotaExceeded(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	code := testTenantCode(t, "it-q")
	rec := e.do(http.MethodPost, "/v1/admin/tenants",
		map[string]any{"name": "Tiny Quota", "code": code,
			"quota": map[string]any{"max_devices": 1}}, authHdr(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create tenant = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var created struct {
		TenantID string `json:"tenant_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if status, raw := e.registerDevice(t, token, created.TenantID, testTenantCode(t, "it-d"), "First"); status != http.StatusCreated {
		t.Fatalf("first device = %d (body=%q)", status, raw)
	}
	rec = e.do(http.MethodPost, "/v1/admin/devices?tenant_id="+created.TenantID, map[string]any{
		"device_code": testTenantCode(t, "it-d"),
		"name":        "Second",
		"device_type": "plc",
		"auth_type":   "token",
	}, authHdr(token))
	wantErr(t, rec, http.StatusForbidden, "11010")
}

// TestDeviceSoftDeleteRetires: DELETE soft-retires the device (204), the
// ledger marks it RETIRED and releases the quota slot (design/32 2.4).
func TestDeviceSoftDeleteRetires(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)

	status, raw := e.registerDevice(t, token, tenantID, testTenantCode(t, "it-del"), "To Retire")
	if status != http.StatusCreated {
		t.Fatalf("register = %d (body=%q)", status, raw)
	}
	deviceID, _ := decodeDeviceRegister(t, raw)

	rec := e.do(http.MethodDelete, "/v1/admin/devices/"+deviceID, nil, authHdr(token))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var dbStatus string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status FROM adc_devices WHERE id = $1::uuid`, deviceID).Scan(&dbStatus); err != nil {
		t.Fatalf("retired device row missing: %v", err)
	}
	if dbStatus != "RETIRED" {
		t.Fatalf("status = %q, want RETIRED", dbStatus)
	}
	// A follow-up patch on the retired device answers 404 code 11001.
	rec = e.do(http.MethodPatch, "/v1/admin/devices/"+deviceID, map[string]any{
		"op": "freeze", "change_reason": "zombie",
	}, authHdr(token))
	wantErr(t, rec, http.StatusNotFound, "11001")
}

// TestDeviceToolRiskConfig covers PATCH /v1/admin/devices/{id}/tools
// (design/33 3.1.11, FR-006): batch risk/enable configuration, the
// mandatory change_reason, the downgrade second-confirmation header
// (X-ADC-Confirm) and partial failure for unknown tool names (11005 in
// failed[], rest of the batch still applies).
func TestDeviceToolRiskConfig(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)
	code := testTenantCode(t, "it-risk")

	status, raw := e.registerDevice(t, token, tenantID, code, "Risk Target")
	if status != http.StatusCreated {
		t.Fatalf("register = %d (body=%q)", status, raw)
	}
	deviceID, _ := decodeDeviceRegister(t, raw)
	// Seed two tool rows (tool metadata is reported by the device and
	// persisted by the data plane; the ledger rows are pre-seeded here).
	_, err := e.pool.Exec(context.Background(), `INSERT INTO adc_device_tools
		(tenant_id, device_id, tool_name, description, input_schema, risk_level, is_enabled)
		VALUES ($1::uuid, $2::uuid, 'set_rpm', 'Set RPM', '{"type":"object"}', 1, TRUE),
		       ($1::uuid, $2::uuid, 'stop_line', 'Stop line', '{"type":"object"}', 3, TRUE)`,
		tenantID, deviceID)
	if err != nil {
		t.Fatalf("seed tools: %v", err)
	}

	// Batch: upgrade set_rpm 1->2 (no confirmation needed) and fail one
	// unknown tool name (partial update, design/33 3.1.11).
	rec := e.do(http.MethodPatch, "/v1/admin/devices/"+deviceID+"/tools", map[string]any{
		"changes": []map[string]any{
			{"name": "set_rpm", "risk_level": 2, "is_enabled": true},
			{"name": "ghost_tool", "risk_level": 0, "is_enabled": true},
		},
		"change_reason": "safety review",
	}, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("tool patch = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Changed int `json:"changed"`
		Failed  []struct {
			Name string `json:"name"`
			Code string `json:"code"`
		} `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tool patch: %v", err)
	}
	if resp.Changed != 1 || len(resp.Failed) != 1 || resp.Failed[0].Name != "ghost_tool" || resp.Failed[0].Code != "11005" {
		t.Fatalf("tool patch response = %+v", resp)
	}
	var dbLevel int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT risk_level FROM adc_device_tools WHERE device_id = $1::uuid AND tool_name = 'set_rpm'`,
		deviceID).Scan(&dbLevel); err != nil || dbLevel != 2 {
		t.Fatalf("risk level after patch = %d (err=%v)", dbLevel, err)
	}

	// Downgrade 2->0 without X-ADC-Confirm -> 400 code 10001 (FR-006
	// second confirmation, HBR-5).
	rec = e.do(http.MethodPatch, "/v1/admin/devices/"+deviceID+"/tools", map[string]any{
		"changes":       []map[string]any{{"name": "set_rpm", "risk_level": 0}},
		"change_reason": "downgrade attempt",
	}, authHdr(token))
	wantErr(t, rec, http.StatusBadRequest, "10001")

	// Downgrade with the confirmation header -> 200 and level 0.
	hdr := authHdr(token)
	hdr["X-ADC-Confirm"] = "true"
	rec = e.do(http.MethodPatch, "/v1/admin/devices/"+deviceID+"/tools", map[string]any{
		"changes":       []map[string]any{{"name": "set_rpm", "risk_level": 0}},
		"change_reason": "reviewed downgrade",
	}, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed downgrade = %d (body=%q)", rec.Code, rec.Body.String())
	}

	// Missing change_reason -> 400 code 10001.
	rec = e.do(http.MethodPatch, "/v1/admin/devices/"+deviceID+"/tools", map[string]any{
		"changes": []map[string]any{{"name": "set_rpm", "risk_level": 2}},
	}, authHdr(token))
	wantErr(t, rec, http.StatusBadRequest, "10001")
}
