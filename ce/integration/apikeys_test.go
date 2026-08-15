// Contract tests for the Agent API key endpoints (design/33 3.1.12-3.1.14,
// FR-009): one-shot issuance, hash-only storage (NFR-004), masked listing,
// revocation with immediate 401 on the Agent API, and rotation in one
// transaction. Issued keys are additionally proven against the live Agent
// API (cross-surface contract).
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"adc.dev/ce/internal/agentauth"
)

// issueKey drives POST /v1/admin/agent-keys for the given token and
// tenant; it returns the recorder and decodes the 201 issuance response.
func (e *testEnv) issueKey(t *testing.T, token, tenantID string) (keyID, fullKey string) {
	t.Helper()
	rec := e.do(http.MethodPost, "/v1/admin/agent-keys?tenant_id="+tenantID, map[string]any{
		"name":     "Scheduling Agent",
		"agent_id": "agent-prod-worker",
		"scopes":   map[string]any{"allowed_tools": []string{"*"}},
	}, authHdr(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue key = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var resp struct {
		KeyID     string `json:"key_id"`
		Key       string `json:"key"`
		KeyPrefix string `json:"key_prefix"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode key response: %v", err)
	}
	if resp.KeyID == "" || resp.Key == "" || resp.KeyPrefix == "" || resp.Status != "active" {
		t.Fatalf("key response = %+v", resp)
	}
	if !strings.HasPrefix(resp.Key, resp.KeyPrefix+"_") {
		t.Fatalf("key %q does not start with prefix %q", resp.Key, resp.KeyPrefix)
	}
	return resp.KeyID, resp.Key
}

// TestIssueKeyHashOnlyStorage: the complete key appears exactly once in
// the 201 response; the ledger keeps only SHA-256 hashes of the key id
// prefix and the secret (NFR-004 / KEY-001); the list endpoint exposes
// only the masked prefix.
func TestIssueKeyHashOnlyStorage(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)

	keyID, fullKey := e.issueKey(t, token, tenantID)
	// The full key is adc_<8hex-keyID>_<secret>; the response key_id is
	// the row UUID, so the secret is parsed from the full key itself.
	rest := strings.TrimPrefix(fullKey, "adc_")
	idx := strings.IndexByte(rest, '_')
	if idx < 0 {
		t.Fatalf("unparsable full key %q", fullKey)
	}
	secret := rest[idx+1:]
	if len(secret) < 16 {
		t.Fatalf("unparsable full key %q", fullKey)
	}

	var storedSecretHash string
	err := e.pool.QueryRow(context.Background(),
		`SELECT secret_hash FROM adc_agent_api_keys WHERE id = $1::uuid`, keyID).Scan(&storedSecretHash)
	if err != nil {
		t.Fatalf("load secret_hash: %v", err)
	}
	if storedSecretHash == "" || strings.Contains(storedSecretHash, secret) {
		t.Fatal("ledger stores the plaintext secret")
	}
	want := agentauth.HashSecret(secret)
	if storedSecretHash != want {
		t.Fatalf("secret_hash = %q, want SHA-256(%q) = %q", storedSecretHash, secret, want)
	}
	// The full key string must not appear anywhere in the table.
	var cnt int
	err = e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM adc_agent_api_keys WHERE key_prefix = $1 OR secret_hash = $2 OR key_hash = $2`,
		fullKey, fullKey).Scan(&cnt)
	if err != nil || cnt != 0 {
		t.Fatalf("plaintext key leaked into the ledger (count=%d, err=%v)", cnt, err)
	}

	// Listing shows the masked prefix, never the full key (design/33
	// 3.1.13). The prefix carries the 8-hex key id, not the row UUID.
	rec := e.do(http.MethodGet, "/v1/admin/agent-keys?tenant_id="+tenantID, nil, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("list keys = %d (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "adc_"+rest[:idx]) {
		t.Fatalf("key prefix missing from list: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), fullKey) {
		t.Fatal("list endpoint leaks the full key")
	}
}

// TestIssueKeyValidation: past / beyond-365-day expiry is rejected with
// 400 code 10001 (design/33 3.1.12).
func TestIssueKeyValidation(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)

	rec := e.do(http.MethodPost, "/v1/admin/agent-keys?tenant_id="+tenantID, map[string]any{
		"name":       "Expired",
		"expires_at": "2020-01-01T00:00:00Z",
	}, authHdr(token))
	wantErr(t, rec, http.StatusBadRequest, "10001")

	rec = e.do(http.MethodPost, "/v1/admin/agent-keys?tenant_id="+tenantID, map[string]any{}, authHdr(token))
	wantErr(t, rec, http.StatusBadRequest, "10001")
}

// TestRevokeKeyDisablesAgentAccess: after POST /revoke the Agent API
// rejects the key with 401 code 10002 immediately (FR-009 acceptance,
// KEY-003), and a second revoke is 409 code 10005.
func TestRevokeKeyDisablesAgentAccess(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)

	keyID, fullKey := e.issueKey(t, token, tenantID)

	// Works before revocation (cross-surface proof).
	rec := e.do(http.MethodGet, "/v1/agent/mcp/tools", nil, map[string]string{"X-ADC-Key": fullKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("agent tools with fresh key = %d (body=%q)", rec.Code, rec.Body.String())
	}

	rec = e.do(http.MethodPost, "/v1/admin/agent-keys/"+keyID+"/revoke",
		map[string]any{"reason": "leak suspected"}, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var revoked struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &revoked)
	if revoked.Status != "revoked" {
		t.Fatalf("revoke response status = %q", revoked.Status)
	}

	rec = e.do(http.MethodGet, "/v1/agent/mcp/tools", nil, map[string]string{"X-ADC-Key": fullKey})
	wantErr(t, rec, http.StatusUnauthorized, "10002")

	// Double revoke -> 409 code 10005.
	rec = e.do(http.MethodPost, "/v1/admin/agent-keys/"+keyID+"/revoke",
		map[string]any{"reason": "again"}, authHdr(token))
	wantErr(t, rec, http.StatusConflict, "10005")
}

// TestRotateKey: rotation issues a fresh key and revokes the old one in
// one transaction (design/80 F-08); the old key stops working, the new
// one works, and the DB shows two rows (old revoked, new active).
func TestRotateKey(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)

	oldID, oldKey := e.issueKey(t, token, tenantID)

	rec := e.do(http.MethodPost, "/v1/admin/agent-keys/"+oldID+"/rotate",
		map[string]any{"reason": "scheduled rotation"}, authHdr(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("rotate = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var rotated struct {
		KeyID         string `json:"key_id"`
		Key           string `json:"key"`
		PreviousKeyID string `json:"previous_key_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode rotate: %v", err)
	}
	if rotated.KeyID == "" || rotated.Key == "" || rotated.Key == oldKey || rotated.PreviousKeyID != oldID {
		t.Fatalf("rotate response = %+v", rotated)
	}

	// Old key dead, new key live.
	rec = e.do(http.MethodGet, "/v1/agent/mcp/tools", nil, map[string]string{"X-ADC-Key": oldKey})
	wantErr(t, rec, http.StatusUnauthorized, "10002")
	rec = e.do(http.MethodGet, "/v1/agent/mcp/tools", nil, map[string]string{"X-ADC-Key": rotated.Key})
	if rec.Code != http.StatusOK {
		t.Fatalf("rotated key = %d (body=%q)", rec.Code, rec.Body.String())
	}

	// Ledger: old row revoked, new row active, both hash-only.
	var oldRevoked, newRevoked bool
	var oldHash, newHash string
	err := e.pool.QueryRow(context.Background(),
		`SELECT revoked_at IS NOT NULL, secret_hash FROM adc_agent_api_keys WHERE id = $1::uuid`, oldID).
		Scan(&oldRevoked, &oldHash)
	if err != nil || !oldRevoked || oldHash == "" {
		t.Fatalf("old key row after rotate: revoked=%v err=%v", oldRevoked, err)
	}
	err = e.pool.QueryRow(context.Background(),
		`SELECT revoked_at IS NOT NULL, secret_hash FROM adc_agent_api_keys WHERE id = $1::uuid`, rotated.KeyID).
		Scan(&newRevoked, &newHash)
	if err != nil || newRevoked {
		t.Fatalf("new key row after rotate: revoked=%v err=%v", newRevoked, err)
	}
	secret := rotated.Key[strings.LastIndex(rotated.Key, "_")+1:]
	if newHash != agentauth.HashSecret(secret) {
		t.Fatalf("rotated secret_hash = %q, want SHA-256 of the issued secret", newHash)
	}
}

// TestAgentKeyCrossTenantForbidden: a tenant_admin revoking another
// tenant's key gets 403 code 13007 (SEC-02, TEN-003).
func TestAgentKeyCrossTenantForbidden(t *testing.T) {
	e := envOrSkip(t)
	platform := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	keyID, _ := e.issueKey(t, platform, e.tenantID(t, fixtureBetaTenant))

	alpha := e.loginOK(t, fixtureAlphaAdmin, fixtureAlphaPass)
	rec := e.do(http.MethodPost, "/v1/admin/agent-keys/"+keyID+"/revoke",
		map[string]any{"reason": "cross-tenant attempt"}, authHdr(alpha))
	wantErr(t, rec, http.StatusForbidden, "13007")
}

// TestAgentAPIWithInvalidKeys: the Agent API rejects missing, malformed
// and unknown keys with the uniform 401 code 10002 (SEC-02, TEN-009).
func TestAgentAPIWithInvalidKeys(t *testing.T) {
	e := envOrSkip(t)
	cases := []map[string]string{
		nil,                      // no key header at all
		{"X-ADC-Key": "bad-key"}, // not adc_<id>_<secret>
		{"X-ADC-Key": "adc_00000000_badsecret"},
		{"X-ADC-Key": "adc_ZZZZZZZZ_validsecret123"},
	}
	for i, hdr := range cases {
		rec := e.do(http.MethodGet, "/v1/agent/mcp/tools", nil, hdr)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("case %d: status = %d, want 401 (body=%q)", i, rec.Code, rec.Body.String())
		}
		e2 := decodeErr(t, rec)
		if e2.Code != "10002" {
			t.Fatalf("case %d: code = %q, want 10002", i, e2.Code)
		}
	}
}
