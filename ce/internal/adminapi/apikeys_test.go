package adminapi

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"adc.dev/ce/internal/agentauth"
)

var apiKeyTokenRe = regexp.MustCompile(`^adc_([a-f0-9]{8})_([a-f0-9]{64})$`)

func TestIssueKeyPlaintextOnceHashesStored(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodPost, "/v1/admin/agent-keys", tok, map[string]any{
		"name":   "排产调度 Agent",
		"scopes": map[string]any{"allowed_tools": []string{"*"}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out apiKeyResponse
	env.decode(t, rec, &out)
	m := apiKeyTokenRe.FindStringSubmatch(out.Key)
	if m == nil {
		t.Fatalf("key %q does not match adc_<8hex>_<64hex>", out.Key)
	}
	keyID, secret := m[1], m[2]
	if out.KeyPrefix != "adc_"+keyID {
		t.Fatalf("key_prefix = %q, want adc_%s", out.KeyPrefix, keyID)
	}
	if out.Status != apiKeyStatusActive || out.Name != "排产调度 Agent" {
		t.Fatalf("unexpected key: %+v", out)
	}
	// storage keeps only SHA-256 hashes, exactly as the auth layer builds them.
	env.store.keys.mu.Lock()
	rec2 := env.store.keys.keys[out.KeyID]
	env.store.keys.mu.Unlock()
	if rec2.keyHash != agentauth.HashKeyID(keyID) {
		t.Fatalf("keyHash = %q, want %q", rec2.keyHash, agentauth.HashKeyID(keyID))
	}
	if rec2.secretHash != agentauth.HashSecret(secret) {
		t.Fatalf("secretHash = %q, want %q", rec2.secretHash, agentauth.HashSecret(secret))
	}
	if strings.Contains(rec2.keyHash, secret) || strings.Contains(rec2.secretHash, secret) {
		t.Fatal("plaintext secret leaked into stored hashes")
	}
	// the list endpoint never returns the key itself.
	listRec := env.do(http.MethodGet, "/v1/admin/agent-keys", tok, nil)
	if strings.Contains(listRec.Body.String(), secret) {
		t.Fatalf("key list leaks the secret: %s", listRec.Body.String())
	}
}

func TestIssueKeyValidation(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	bad := []map[string]any{
		{"name": ""},
		{"name": "k", "expires_at": "2026-08-01T00:00:00Z"},                       // in the past
		{"name": "k", "expires_at": "2027-09-01T00:00:00Z"},                       // beyond 365d
		{"name": "k", "scopes": map[string]any{"allowed_tools": []string{"a b"}}}, // bad charset
		{"name": strings.Repeat("x", 256)},
	}
	for i, body := range bad {
		rec := env.do(http.MethodPost, "/v1/admin/agent-keys", tok, body)
		env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
		t.Logf("case %d rejected as expected", i)
	}
}

func TestIssueKeyExpiryWindow(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	expires := fixedNow.Add(90 * 24 * time.Hour).UTC().Format(time.RFC3339)
	rec := env.do(http.MethodPost, "/v1/admin/agent-keys", tok, map[string]any{
		"name": "k", "expires_at": expires,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out apiKeyResponse
	env.decode(t, rec, &out)
	if out.ExpiresAt == nil || !out.ExpiresAt.Equal(fixedNow.Add(90*24*time.Hour)) {
		t.Fatalf("expires_at = %v", out.ExpiresAt)
	}
}

func TestIssueKeySuspendedTenant(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	pok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPatch, "/v1/admin/tenants/"+tr.TenantID, pok, map[string]any{
		"status": "suspended", "change_reason": "x",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("suspend: %d", rec.Code)
	}
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec = env.do(http.MethodPost, "/v1/admin/agent-keys", tok, map[string]any{"name": "k"})
	env.assertError(t, rec, http.StatusForbidden, codeTenantSuspended)
}

func TestListKeysMasksAndPaginates(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	for i := 0; i < 3; i++ {
		rec := env.do(http.MethodPost, "/v1/admin/agent-keys", tok, map[string]any{"name": "k"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("issue: %d; body: %s", rec.Code, rec.Body.String())
		}
	}
	rec := env.do(http.MethodGet, "/v1/admin/agent-keys?page=2&page_size=2", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out apiKeyListResponse
	env.decode(t, rec, &out)
	if out.Total != 3 || len(out.Items) != 1 {
		t.Fatalf("pagination: total=%d items=%d", out.Total, len(out.Items))
	}
	if out.Items[0].Key != "" || !strings.HasPrefix(out.Items[0].KeyPrefix, "adc_") {
		t.Fatalf("list item must mask the key: %+v", out.Items[0])
	}
}

func TestRevokeKey(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	var issued apiKeyResponse
	rec := env.do(http.MethodPost, "/v1/admin/agent-keys", tok, map[string]any{"name": "k"})
	env.decode(t, rec, &issued)

	// reason is mandatory.
	rec = env.do(http.MethodPost, "/v1/admin/agent-keys/"+issued.KeyID+"/revoke", tok, map[string]any{})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)

	rec = env.do(http.MethodPost, "/v1/admin/agent-keys/"+issued.KeyID+"/revoke", tok, map[string]any{
		"reason": "流程下线",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var revoked apiKeyRevokeResponse
	env.decode(t, rec, &revoked)
	if revoked.Status != apiKeyStatusRevoked || revoked.RevokedAt == nil {
		t.Fatalf("unexpected revoke response: %+v", revoked)
	}
	// double revoke is a conflict.
	rec = env.do(http.MethodPost, "/v1/admin/agent-keys/"+issued.KeyID+"/revoke", tok, map[string]any{
		"reason": "again",
	})
	env.assertError(t, rec, http.StatusConflict, codeConflict)

	// unknown key.
	rec = env.do(http.MethodPost, "/v1/admin/agent-keys/"+uuidOf(1)+"/revoke", tok, map[string]any{
		"reason": "x",
	})
	env.assertError(t, rec, http.StatusNotFound, codeNotFound)

	// malformed key id.
	rec = env.do(http.MethodPost, "/v1/admin/agent-keys/nope/revoke", tok, map[string]any{
		"reason": "x",
	})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)

	op, ok := env.audit.last()
	if !ok || op.Action != "apikey.revoke" || op.Reason != "again" && op.Reason != "流程下线" {
		t.Fatalf("audit = %+v", op)
	}
}

func TestRotateKey(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	var old apiKeyResponse
	rec := env.do(http.MethodPost, "/v1/admin/agent-keys", tok, map[string]any{
		"name": "k", "scopes": map[string]any{"allowed_tools": []string{"cnc-01__*"}},
	})
	env.decode(t, rec, &old)

	rec = env.do(http.MethodPost, "/v1/admin/agent-keys/"+old.KeyID+"/rotate", tok, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out apiKeyRotateResponse
	env.decode(t, rec, &out)
	if out.PreviousKeyID != old.KeyID {
		t.Fatalf("previous_key_id = %q, want %q", out.PreviousKeyID, old.KeyID)
	}
	if out.Key == "" || out.Key == old.Key || out.KeyID == old.KeyID {
		t.Fatalf("rotate must yield a fresh key: %+v", out)
	}
	if len(out.Scopes.AllowedTools) != 1 || out.Scopes.AllowedTools[0] != "cnc-01__*" {
		t.Fatalf("scopes not inherited: %+v", out.Scopes)
	}
	// the old key is revoked atomically.
	oldRec := env.do(http.MethodGet, "/v1/admin/agent-keys", tok, nil)
	var list apiKeyListResponse
	env.decode(t, oldRec, &list)
	for _, item := range list.Items {
		if item.KeyID == old.KeyID && item.Status != apiKeyStatusRevoked {
			t.Fatalf("old key not revoked after rotate: %+v", item)
		}
	}
	// rotating the already-revoked key conflicts.
	rec = env.do(http.MethodPost, "/v1/admin/agent-keys/"+old.KeyID+"/rotate", tok, nil)
	env.assertError(t, rec, http.StatusConflict, codeConflict)
}

func TestApiKeysCrossTenantDenied(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTenant(t, "acme-a", "Acme A", nil)
	b := env.createTenant(t, "acme-b", "Acme B", nil)
	btok := env.token(t, []string{"tenant_admin"}, b.TenantID)
	var issued apiKeyResponse
	rec := env.do(http.MethodPost, "/v1/admin/agent-keys", btok, map[string]any{"name": "k"})
	env.decode(t, rec, &issued)

	atok := env.token(t, []string{"tenant_admin"}, a.TenantID)
	rec = env.do(http.MethodPost, "/v1/admin/agent-keys/"+issued.KeyID+"/revoke", atok, map[string]any{
		"reason": "x",
	})
	env.assertError(t, rec, http.StatusForbidden, codeCrossTenant)
}

func TestApiKeysUnauthenticated(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodPost, "/v1/admin/agent-keys", "", map[string]any{"name": "k"})
	env.assertError(t, rec, http.StatusUnauthorized, codeUnauthorized)
}
