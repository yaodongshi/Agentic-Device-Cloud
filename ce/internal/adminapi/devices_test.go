package adminapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"adc.dev/ce/internal/auth"
)

func registerBody(code, authType string) map[string]any {
	return map[string]any{
		"device_code": code,
		"name":        "一号车床",
		"device_type": "cnc",
		"auth_type":   authType,
		"metadata":    map[string]any{"workshop": "A1"},
	}
}

func TestRegisterDeviceTokenCredentialOnce(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("cnc-lathe-01", "token"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out deviceRegisterResponse
	env.decode(t, rec, &out)
	if out.Credential == nil || out.Credential.Secret == "" {
		t.Fatalf("credential not returned once: %+v", out)
	}
	if out.Status != deviceStatusOffline || out.Metadata["workshop"] != "A1" {
		t.Fatalf("unexpected device: %+v", out)
	}
	// storage keeps a salted one-way hash, never the plaintext.
	env.store.devices.mu.Lock()
	rec2 := env.store.devices.devices[out.DeviceID]
	env.store.devices.mu.Unlock()
	if !strings.HasPrefix(rec2.d.credentialStored, "sha256$") {
		t.Fatalf("stored form = %q, want sha256$...", rec2.d.credentialStored)
	}
	ok, err := auth.VerifySecret(out.Credential.Secret, rec2.d.credentialStored)
	if err != nil || !ok {
		t.Fatalf("stored hash does not verify the issued secret: %v", err)
	}
	if strings.Contains(rec2.d.credentialStored, out.Credential.Secret) {
		t.Fatalf("plaintext leaked into storage: %q", rec2.d.credentialStored)
	}
	// the list endpoint never exposes any credential field.
	listRec := env.do(http.MethodGet, "/v1/admin/devices", tok, nil)
	if strings.Contains(listRec.Body.String(), "secret") {
		t.Fatalf("device list leaks credential: %s", listRec.Body.String())
	}
}

func TestRegisterDeviceHMACStoresKEKEncrypted(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("cnc-lathe-02", "hmac"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out deviceRegisterResponse
	env.decode(t, rec, &out)
	env.store.devices.mu.Lock()
	stored := env.store.devices.devices[out.DeviceID].d.credentialStored
	env.store.devices.mu.Unlock()
	if !strings.HasPrefix(stored, "encv1$") {
		t.Fatalf("stored form = %q, want encv1$...", stored)
	}
	plain, err := auth.DecryptSecret(stored, testKEK)
	if err != nil {
		t.Fatalf("decrypt stored hmac key: %v", err)
	}
	if plain != out.Credential.Secret {
		t.Fatalf("decrypted key %q != issued secret %q", plain, out.Credential.Secret)
	}
}

func TestRegisterDeviceHMACWithoutKEKFailsClosed(t *testing.T) {
	env := newTestEnv(t)
	// KEK nil: hmac registration must fail closed (no degraded issuance).
	srv := NewServer(env.store.tenants, env.store.devices, env.store.keys, env.sessions)
	srv.Now = func() time.Time { return fixedNow }
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := doRaw(srv.Handler(), http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-hmac", "hmac"))
	env.assertError(t, rec, http.StatusInternalServerError, codeInternal)
}

func TestRegisterDeviceMTLS(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-mtls", "mtls"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out deviceRegisterResponse
	env.decode(t, rec, &out)
	if out.Credential == nil || out.Credential.CertURL == nil || out.Credential.Secret != "" {
		t.Fatalf("mtls must return cert_url and no secret: %+v", out.Credential)
	}
}

func TestRegisterDeviceDuplicateCode(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-dup", "token"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first register: %d; body: %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-dup", "token"))
	env.assertError(t, rec, http.StatusConflict, codeDeviceCodeExists)
}

func TestRegisterDeviceValidation(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	bad := []map[string]any{
		{"device_code": "bad code!", "name": "d", "device_type": "cnc", "auth_type": "token"},
		{"device_code": strings.Repeat("x", 129), "name": "d", "device_type": "cnc", "auth_type": "token"},
		{"device_code": "dev-x", "name": "", "device_type": "cnc", "auth_type": "token"},
		{"device_code": "dev-x", "name": "d", "device_type": "", "auth_type": "token"},
		{"device_code": "dev-x", "name": "d", "device_type": "cnc", "auth_type": "oauth2_client_credentials"},
		{"device_code": "dev-x", "name": "d", "device_type": "cnc", "auth_type": "token", "group_ids": []string{"nope"}},
	}
	for i, body := range bad {
		rec := env.do(http.MethodPost, "/v1/admin/devices", tok, body)
		env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
		t.Logf("case %d rejected as expected", i)
	}
}

func TestRegisterDeviceQuotaExceeded(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", &quotaRequest{MaxDevices: intPtr(1)})
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-a", "token"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first register: %d; body: %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-b", "token"))
	env.assertError(t, rec, http.StatusForbidden, codeDeviceQuota)
}

func TestRegisterDeviceMissingTenant(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPost, "/v1/admin/devices?tenant_id="+uuidOf(4242), tok, registerBody("dev-x", "token"))
	env.assertError(t, rec, http.StatusNotFound, codeTenantNotFound)
}

func TestListDevicesFiltersAndPagination(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	robotBody := registerBody("robot-01", "token")
	robotBody["device_type"] = "robot"
	for _, body := range []map[string]any{
		registerBody("cnc-01", "token"),
		registerBody("cnc-02", "token"),
		robotBody,
	} {
		rec := env.do(http.MethodPost, "/v1/admin/devices", tok, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("register %v: %d; body: %s", body["device_code"], rec.Code, rec.Body.String())
		}
	}
	rec := env.do(http.MethodGet, "/v1/admin/devices?device_type=cnc", tok, nil)
	var out deviceListResponse
	env.decode(t, rec, &out)
	if out.Total != 2 {
		t.Fatalf("device_type filter total = %d, want 2", out.Total)
	}
	rec = env.do(http.MethodGet, "/v1/admin/devices?keyword=robot", tok, nil)
	env.decode(t, rec, &out)
	if out.Total != 1 || out.Items[0].DeviceCode != "robot-01" {
		t.Fatalf("keyword filter: %+v", out)
	}
	rec = env.do(http.MethodGet, "/v1/admin/devices?page=2&page_size=2", tok, nil)
	env.decode(t, rec, &out)
	if out.Total != 3 || len(out.Items) != 1 {
		t.Fatalf("pagination: total=%d items=%d", out.Total, len(out.Items))
	}
	rec = env.do(http.MethodGet, "/v1/admin/devices?status=bogus", tok, nil)
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
}

func TestListDevicesPlatformAdminNeedsTenantID(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodGet, "/v1/admin/devices", tok, nil)
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
}

func TestListDevicesCrossTenantDenied(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTenant(t, "acme-a", "Acme A", nil)
	b := env.createTenant(t, "acme-b", "Acme B", nil)
	tok := env.token(t, []string{"tenant_admin"}, a.TenantID)
	rec := env.do(http.MethodGet, "/v1/admin/devices?tenant_id="+b.TenantID, tok, nil)
	env.assertError(t, rec, http.StatusForbidden, codeCrossTenant)
}

func TestPatchDeviceFreezeUnfreeze(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	var reg deviceRegisterResponse
	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-f", "token"))
	env.decode(t, rec, &reg)
	id := reg.DeviceID

	patch := func(body map[string]any) *httptest.ResponseRecorder {
		return env.do(http.MethodPatch, "/v1/admin/devices/"+id, tok, body)
	}
	rec = patch(map[string]any{"op": "freeze", "change_reason": "安全检查"})
	if rec.Code != http.StatusOK {
		t.Fatalf("freeze: %d; body: %s", rec.Code, rec.Body.String())
	}
	var out devicePatchResponse
	env.decode(t, rec, &out)
	if out.Status != deviceStatusFrozen {
		t.Fatalf("status = %q, want frozen", out.Status)
	}
	// unfreezing a non-frozen device is a bad request.
	rec = patch(map[string]any{"op": "unfreeze", "change_reason": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("unfreeze: %d; body: %s", rec.Code, rec.Body.String())
	}
	env.decode(t, rec, &out)
	if out.Status != deviceStatusOffline {
		t.Fatalf("status = %q, want offline", out.Status)
	}
	rec = patch(map[string]any{"op": "unfreeze", "change_reason": "x"})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)

	// reset on frozen is refused with 11003.
	rec = patch(map[string]any{"op": "freeze", "change_reason": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("freeze 2: %d", rec.Code)
	}
	rec = patch(map[string]any{"op": "reset_credential", "change_reason": "x"})
	env.assertError(t, rec, http.StatusForbidden, codeDeviceFrozen)

	// audit trail carries the change reason on every successful op.
	op, ok := env.audit.last()
	if !ok || !strings.HasPrefix(op.Action, "device.") || op.Reason == "" {
		t.Fatalf("audit = %+v, want device.* with reason", op)
	}
}

func TestPatchDeviceResetCredential(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	var reg deviceRegisterResponse
	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-r", "token"))
	env.decode(t, rec, &reg)
	env.store.devices.mu.Lock()
	oldStored := env.store.devices.devices[reg.DeviceID].d.credentialStored
	oldVersion := env.store.devices.devices[reg.DeviceID].d.CredentialVersion
	env.store.devices.mu.Unlock()

	rec = env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID, tok, map[string]any{
		"op": "reset_credential", "change_reason": "工程变更",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out devicePatchResponse
	env.decode(t, rec, &out)
	if out.Credential == nil || out.Credential.Secret == "" {
		t.Fatalf("reset must return the new credential once: %+v", out)
	}
	env.store.devices.mu.Lock()
	newStored := env.store.devices.devices[reg.DeviceID].d.credentialStored
	newVersion := env.store.devices.devices[reg.DeviceID].d.CredentialVersion
	env.store.devices.mu.Unlock()
	if newStored == oldStored || newVersion != oldVersion+1 {
		t.Fatalf("credential not rotated: stored changed=%v version=%d->%d", newStored != oldStored, oldVersion, newVersion)
	}
	ok, err := auth.VerifySecret(out.Credential.Secret, newStored)
	if err != nil || !ok {
		t.Fatalf("new stored hash does not verify: %v", err)
	}
}

func TestPatchDeviceRevokeCredential(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	var reg deviceRegisterResponse
	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-v", "token"))
	env.decode(t, rec, &reg)
	rec = env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID, tok, map[string]any{
		"op": "revoke_credential", "change_reason": "外包交接",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out devicePatchResponse
	env.decode(t, rec, &out)
	if out.Credential != nil {
		t.Fatalf("revoke must not return a credential: %+v", out.Credential)
	}
	env.store.devices.mu.Lock()
	stored := env.store.devices.devices[reg.DeviceID].d.credentialStored
	env.store.devices.mu.Unlock()
	if !strings.HasPrefix(stored, "revoked$") {
		t.Fatalf("stored form = %q, want revoked$ tombstone", stored)
	}
}

func TestPatchDeviceUpdateMeta(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	var reg deviceRegisterResponse
	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-m", "token"))
	env.decode(t, rec, &reg)
	name := "一号车床（A1 车间）"
	rec = env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID, tok, map[string]any{
		"op": "update_meta", "change_reason": "改名", "name": name,
		"metadata": map[string]any{"workshop": "A2"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out devicePatchResponse
	env.decode(t, rec, &out)
	if out.Name != name {
		t.Fatalf("name = %q", out.Name)
	}
	// change_reason is mandatory on every op.
	rec = env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID, tok, map[string]any{"op": "update_meta", "name": "x"})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	// unknown op.
	rec = env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID, tok, map[string]any{"op": "explode", "change_reason": "x"})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
}

func TestPatchDeviceNotFoundAndCrossTenant(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	other := env.createTenant(t, "other", "Other", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodPatch, "/v1/admin/devices/"+uuidOf(777), tok, map[string]any{
		"op": "freeze", "change_reason": "x",
	})
	env.assertError(t, rec, http.StatusNotFound, codeDeviceNotFound)
	rec = env.do(http.MethodPatch, "/v1/admin/devices/not-a-uuid", tok, map[string]any{
		"op": "freeze", "change_reason": "x",
	})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)

	// a tenant_admin cannot reach another tenant's device.
	otok := env.token(t, []string{"tenant_admin"}, other.TenantID)
	var reg deviceRegisterResponse
	rec = env.do(http.MethodPost, "/v1/admin/devices", otok, registerBody("other-dev", "token"))
	env.decode(t, rec, &reg)
	rec = env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID, tok, map[string]any{
		"op": "freeze", "change_reason": "x",
	})
	env.assertError(t, rec, http.StatusForbidden, codeCrossTenant)
}

func TestDeleteDeviceSoftDeleteBurnsCode(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", &quotaRequest{MaxDevices: intPtr(1)})
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	var reg deviceRegisterResponse
	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-del", "token"))
	env.decode(t, rec, &reg)

	rec = env.do(http.MethodDelete, "/v1/admin/devices/"+reg.DeviceID, tok, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	// deleted devices are gone.
	rec = env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID, tok, map[string]any{
		"op": "freeze", "change_reason": "x",
	})
	env.assertError(t, rec, http.StatusNotFound, codeDeviceNotFound)
	// the device code stays burned forever (FR-011)...
	rec = env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-del", "token"))
	env.assertError(t, rec, http.StatusConflict, codeDeviceCodeExists)
	// ...but the quota slot is released for a different code.
	rec = env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-new", "token"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register after delete: %d; body: %s", rec.Code, rec.Body.String())
	}
	// deleting twice is a 404.
	rec = env.do(http.MethodDelete, "/v1/admin/devices/"+reg.DeviceID, tok, nil)
	env.assertError(t, rec, http.StatusNotFound, codeDeviceNotFound)
}

func TestDevicesRoleMatrixAndAuth(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)

	// unauthenticated: 401.
	rec := env.do(http.MethodGet, "/v1/admin/devices", "", nil)
	env.assertError(t, rec, http.StatusUnauthorized, codeUnauthorized)

	// approver may read the device list, but cannot register or patch.
	app := env.token(t, []string{"approver"}, tr.TenantID)
	rec = env.do(http.MethodGet, "/v1/admin/devices", app, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approver list: %d; body: %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodPost, "/v1/admin/devices", app, registerBody("dev-x", "token"))
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)
	rec = env.do(http.MethodDelete, "/v1/admin/devices/"+uuidOf(1), app, nil)
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)

	// auditor: no device access at all.
	aud := env.token(t, []string{"auditor"}, tr.TenantID)
	rec = env.do(http.MethodGet, "/v1/admin/devices", aud, nil)
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)
}

// doRaw drives a separate handler instance (used by the no-KEK fixture).
func doRaw(h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
