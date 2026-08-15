package agentauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"adc.dev/ce/internal/httpx"
)

func TestAuthorize(t *testing.T) {
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		if !ok {
			http.Error(w, "missing principal", http.StatusInternalServerError)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, p)
	})

	tests := []struct {
		name       string
		headers    map[string]string
		validator  ApiKeyValidator
		wantStatus int
		wantCode   string // business error code when an error body is expected
	}{
		{
			name:       "x-adc-key header",
			headers:    map[string]string{HeaderXADCKey: testToken(testKeyID, testSecret)},
			validator:  NewValidator(storeWith(validKey())),
			wantStatus: http.StatusOK,
		},
		{
			name:       "bearer header",
			headers:    map[string]string{"Authorization": "Bearer " + testToken(testKeyID, testSecret)},
			validator:  NewValidator(storeWith(validKey())),
			wantStatus: http.StatusOK,
		},
		{
			name:       "bearer lowercase scheme",
			headers:    map[string]string{"Authorization": "bearer " + testToken(testKeyID, testSecret)},
			validator:  NewValidator(storeWith(validKey())),
			wantStatus: http.StatusOK,
		},
		{
			name:       "x-adc-key wins over bearer",
			headers:    map[string]string{HeaderXADCKey: testToken(testKeyID, testSecret), "Authorization": "Bearer garbage"},
			validator:  NewValidator(storeWith(validKey())),
			wantStatus: http.StatusOK,
		},
		{
			name:       "missing token",
			headers:    nil,
			wantStatus: http.StatusUnauthorized,
			wantCode:   codeUnauthorized,
		},
		{
			name:       "non-bearer authorization scheme",
			headers:    map[string]string{"Authorization": "Basic dXNlcjpwYXNz"},
			wantStatus: http.StatusUnauthorized,
			wantCode:   codeUnauthorized,
		},
		{
			name:       "wrong secret",
			headers:    map[string]string{HeaderXADCKey: testToken(testKeyID, "Yz9kQ2mR7vL4nB8tR6wYxH7")},
			validator:  NewValidator(storeWith(validKey())),
			wantStatus: http.StatusUnauthorized,
			wantCode:   codeUnauthorized,
		},
		{
			name:       "expired key",
			headers:    map[string]string{HeaderXADCKey: testToken(testKeyID, testSecret)},
			validator:  NewValidator(storeWith(keyWith(validKey(), func(k *AgentKey) { k.ExpiresAt = time.Now().Add(-time.Hour) }))),
			wantStatus: http.StatusUnauthorized,
			wantCode:   codeUnauthorized,
		},
		{
			name:       "malformed token",
			headers:    map[string]string{HeaderXADCKey: "adc_bad"},
			validator:  NewValidator(storeWith(validKey())),
			wantStatus: http.StatusUnauthorized,
			wantCode:   codeUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := Authorize(tt.validator)(echo)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/agent/mcp/tools", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode == "" {
				var p Principal
				if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
					t.Fatalf("response is not a Principal: %v", err)
				}
				if p.KeyID != testKeyID || p.TenantID != testTenant || p.AgentID != testAgentID {
					t.Fatalf("principal = %+v, want key %s / tenant %s / agent %s", p, testKeyID, testTenant, testAgentID)
				}
				return
			}
			var body httpx.ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not an error body: %v", err)
			}
			if body.Code != tt.wantCode {
				t.Fatalf("error code = %q, want %q", body.Code, tt.wantCode)
			}
		})
	}
}

func TestAuthorizeStoreError(t *testing.T) {
	validator := NewValidator(&mockKeyStore{err: errStoreDown})
	handler := Authorize(validator)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/agent/mcp/tools", nil)
	req.Header.Set(HeaderXADCKey, testToken(testKeyID, testSecret))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error body: %v", err)
	}
	if body.Code != codeInternal {
		t.Fatalf("error code = %q, want %q", body.Code, codeInternal)
	}
}

func TestFromContext(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("FromContext(empty ctx) ok = true, want false")
	}
	p := &Principal{KeyID: testKeyID, TenantID: testTenant}
	if got, ok := FromContext(WithPrincipal(context.Background(), p)); !ok || got != p {
		t.Fatalf("FromContext(WithPrincipal(...)) = (%+v, %v), want (%+v, true)", got, ok, p)
	}
}
