package adminauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"adc.dev/ce/internal/httpx"
)

func TestPermissionMatrix(t *testing.T) {
	tests := []struct {
		name   string
		roles  []string
		method string
		path   string
		want   bool
	}{
		// RoleAdmin: every Admin API path, every method.
		{name: "admin get tenants", roles: []string{"admin"}, method: http.MethodGet, path: "/v1/admin/tenants", want: true},
		{name: "admin post devices", roles: []string{"admin"}, method: http.MethodPost, path: "/v1/admin/devices", want: true},
		{name: "admin delete agent key", roles: []string{"admin"}, method: http.MethodDelete, path: "/v1/admin/agent-keys/k1", want: true},
		{name: "admin audit logs", roles: []string{"admin"}, method: http.MethodGet, path: "/v1/admin/audit-logs", want: true},
		// design/33 role names map into the admin tier.
		{name: "platform_admin anywhere", roles: []string{"platform_admin"}, method: http.MethodPost, path: "/v1/admin/tenants", want: true},
		{name: "tenant_admin anywhere", roles: []string{"tenant_admin"}, method: http.MethodGet, path: "/v1/admin/usage", want: true},
		// RoleApprover: read-only device list and approval tickets.
		{name: "approver get devices", roles: []string{"approver"}, method: http.MethodGet, path: "/v1/admin/devices", want: true},
		{name: "approver get device tools", roles: []string{"approver"}, method: http.MethodGet, path: "/v1/admin/devices/d1/tools", want: true},
		{name: "approver get tickets", roles: []string{"approver"}, method: http.MethodGet, path: "/v1/admin/approval-tickets", want: true},
		{name: "approver post devices denied", roles: []string{"approver"}, method: http.MethodPost, path: "/v1/admin/devices", want: false},
		{name: "approver patch tools denied", roles: []string{"approver"}, method: http.MethodPatch, path: "/v1/admin/devices/d1/tools", want: false},
		{name: "approver audit denied", roles: []string{"approver"}, method: http.MethodGet, path: "/v1/admin/audit-logs", want: false},
		{name: "approver tenants denied", roles: []string{"approver"}, method: http.MethodGet, path: "/v1/admin/tenants", want: false},
		{name: "approver prefix boundary", roles: []string{"approver"}, method: http.MethodGet, path: "/v1/admin/devicesx", want: false},
		// RoleAuditor: read-only audit logs and usage.
		{name: "auditor get audit logs", roles: []string{"auditor"}, method: http.MethodGet, path: "/v1/admin/audit-logs", want: true},
		{name: "auditor get audit export", roles: []string{"auditor"}, method: http.MethodGet, path: "/v1/admin/audit-logs/export", want: true},
		{name: "auditor get usage", roles: []string{"auditor"}, method: http.MethodGet, path: "/v1/admin/usage", want: true},
		{name: "auditor post audit denied", roles: []string{"auditor"}, method: http.MethodPost, path: "/v1/admin/audit-logs", want: false},
		{name: "auditor delete audit denied", roles: []string{"auditor"}, method: http.MethodDelete, path: "/v1/admin/audit-logs/l1", want: false},
		{name: "auditor devices denied", roles: []string{"auditor"}, method: http.MethodGet, path: "/v1/admin/devices", want: false},
		{name: "auditor patch usage denied", roles: []string{"auditor"}, method: http.MethodPatch, path: "/v1/admin/usage", want: false},
		// Mixed roles: any matching tier grants.
		{name: "admin+approver union", roles: []string{"approver", "admin"}, method: http.MethodPost, path: "/v1/admin/devices", want: true},
		{name: "approver+auditor union", roles: []string{"approver", "auditor"}, method: http.MethodGet, path: "/v1/admin/audit-logs", want: true},
		// Unknown or empty roles fail closed.
		{name: "unknown role denied", roles: []string{"superuser"}, method: http.MethodGet, path: "/v1/admin/tenants", want: false},
		{name: "no roles denied", roles: nil, method: http.MethodGet, path: "/v1/admin/tenants", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Permission(tt.roles, tt.method, tt.path); got != tt.want {
				t.Fatalf("Permission(%v, %s, %s) = %v, want %v", tt.roles, tt.method, tt.path, got, tt.want)
			}
		})
	}
}

func TestExtractToken(t *testing.T) {
	tests := []struct {
		name   string
		auth   string
		cookie string
		want   string
	}{
		{name: "bearer", auth: "Bearer tok123", want: "tok123"},
		{name: "bearer lowercase scheme", auth: "bearer tok123", want: "tok123"},
		{name: "cookie only", cookie: "tok456", want: "tok456"},
		{name: "bearer wins over cookie", auth: "Bearer tok123", cookie: "tok456", want: "tok123"},
		{name: "other scheme ignored", auth: "Basic dXNlcjpwYXNz", want: ""},
		{name: "nothing", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			if tt.cookie != "" {
				req.AddCookie(&http.Cookie{Name: CookieSession, Value: tt.cookie})
			}
			if got := ExtractToken(req); got != tt.want {
				t.Fatalf("ExtractToken = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAuthorize(t *testing.T) {
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		if !ok {
			http.Error(w, "missing principal", http.StatusInternalServerError)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, p)
	})

	validSess := &Session{
		TokenHash: HashToken("valid-token"),
		UserID:    "u_9f8e7d6c",
		TenantID:  "t_1a2b3c4d",
		Roles:     []string{"tenant_admin"},
		ExpiresAt: timeNowPlus(time.Hour),
	}

	tests := []struct {
		name       string
		setup      func() (map[string]string, *mockSessionStore)
		wantStatus int
		wantCode   string
		wantUser   string
	}{
		{
			name: "valid bearer",
			setup: func() (map[string]string, *mockSessionStore) {
				s := newMockSessionStore()
				s.sessions[validSess.TokenHash] = validSess
				return map[string]string{"Authorization": "Bearer valid-token"}, s
			},
			wantStatus: http.StatusOK,
			wantUser:   "u_9f8e7d6c",
		},
		{
			name: "valid cookie",
			setup: func() (map[string]string, *mockSessionStore) {
				s := newMockSessionStore()
				s.sessions[validSess.TokenHash] = validSess
				return nil, s
			},
			wantStatus: http.StatusOK,
			wantUser:   "u_9f8e7d6c",
		},
		{
			name:       "missing token",
			setup:      func() (map[string]string, *mockSessionStore) { return nil, newMockSessionStore() },
			wantStatus: http.StatusUnauthorized,
			wantCode:   codeUnauthorized,
		},
		{
			name: "unknown session",
			setup: func() (map[string]string, *mockSessionStore) {
				return map[string]string{"Authorization": "Bearer ghost"}, newMockSessionStore()
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   codeUnauthorized,
		},
		{
			name: "expired session",
			setup: func() (map[string]string, *mockSessionStore) {
				s := newMockSessionStore()
				return map[string]string{"Authorization": "Bearer ghost"}, s
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   codeUnauthorized,
		},
		{
			name: "store error",
			setup: func() (map[string]string, *mockSessionStore) {
				s := newMockSessionStore()
				s.getErr = errors.New("valkey down")
				return map[string]string{"Authorization": "Bearer valid-token"}, s
			},
			wantStatus: http.StatusInternalServerError,
			wantCode:   codeInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers, store := tt.setup()
			handler := Authorize(store)(echo)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/admin/tenants", nil)
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			// cookie variant
			if tt.name == "valid cookie" {
				req.AddCookie(&http.Cookie{Name: CookieSession, Value: "valid-token"})
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
				if p.UserID != tt.wantUser || p.TenantID != "t_1a2b3c4d" {
					t.Fatalf("principal = %+v, want user %s / tenant t_1a2b3c4d", p, tt.wantUser)
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

func TestRequireRole(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	newReq := func(method, path string, p *Principal) (*httptest.ResponseRecorder, *http.Request) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		if p != nil {
			req = req.WithContext(WithPrincipal(context.Background(), p))
		}
		return rec, req
	}

	tests := []struct {
		name       string
		roles      []Role
		principal  *Principal
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{
			name:   "no principal is 401",
			roles:  []Role{RoleAdmin},
			method: http.MethodGet, path: "/v1/admin/tenants",
			wantStatus: http.StatusUnauthorized, wantCode: codeUnauthorized,
		},
		{
			name:      "admin passes any endpoint",
			roles:     []Role{RoleAdmin},
			principal: &Principal{UserID: "u1", Roles: []string{"platform_admin"}},
			method:    http.MethodPost, path: "/v1/admin/devices",
			wantStatus: http.StatusOK,
		},
		{
			name:      "approver passes approval ticket list",
			roles:     []Role{RoleApprover},
			principal: &Principal{UserID: "u2", Roles: []string{"approver"}},
			method:    http.MethodGet, path: "/v1/admin/approval-tickets",
			wantStatus: http.StatusOK,
		},
		{
			name:      "approver passes device list",
			roles:     []Role{RoleApprover},
			principal: &Principal{UserID: "u2", Roles: []string{"approver"}},
			method:    http.MethodGet, path: "/v1/admin/devices",
			wantStatus: http.StatusOK,
		},
		{
			name:      "approver write denied",
			roles:     []Role{RoleApprover},
			principal: &Principal{UserID: "u2", Roles: []string{"approver"}},
			method:    http.MethodPost, path: "/v1/admin/devices",
			wantStatus: http.StatusForbidden, wantCode: codeForbidden,
		},
		{
			name:      "approver outside matrix denied",
			roles:     []Role{RoleApprover},
			principal: &Principal{UserID: "u2", Roles: []string{"approver"}},
			method:    http.MethodGet, path: "/v1/admin/audit-logs",
			wantStatus: http.StatusForbidden, wantCode: codeForbidden,
		},
		{
			name:      "approver lacking admin tier denied",
			roles:     []Role{RoleAdmin},
			principal: &Principal{UserID: "u2", Roles: []string{"approver"}},
			method:    http.MethodGet, path: "/v1/admin/tenants",
			wantStatus: http.StatusForbidden, wantCode: codeForbidden,
		},
		{
			name:      "auditor passes audit query",
			roles:     []Role{RoleAuditor},
			principal: &Principal{UserID: "u3", Roles: []string{"auditor"}},
			method:    http.MethodGet, path: "/v1/admin/audit-logs",
			wantStatus: http.StatusOK,
		},
		{
			name:      "auditor passes usage",
			roles:     []Role{RoleAuditor},
			principal: &Principal{UserID: "u3", Roles: []string{"auditor"}},
			method:    http.MethodGet, path: "/v1/admin/usage",
			wantStatus: http.StatusOK,
		},
		{
			name:      "auditor write denied",
			roles:     []Role{RoleAuditor},
			principal: &Principal{UserID: "u3", Roles: []string{"auditor"}},
			method:    http.MethodPost, path: "/v1/admin/audit-logs",
			wantStatus: http.StatusForbidden, wantCode: codeForbidden,
		},
		{
			name:      "auditor outside matrix denied",
			roles:     []Role{RoleAuditor},
			principal: &Principal{UserID: "u3", Roles: []string{"auditor"}},
			method:    http.MethodGet, path: "/v1/admin/devices",
			wantStatus: http.StatusForbidden, wantCode: codeForbidden,
		},
		{
			name:      "multi-tier principal passes when one tier matches",
			roles:     []Role{RoleAuditor},
			principal: &Principal{UserID: "u4", Roles: []string{"tenant_admin", "auditor"}},
			method:    http.MethodGet, path: "/v1/admin/audit-logs",
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, req := newReq(tt.method, tt.path, tt.principal)
			RequireRole(tt.roles...)(ok).ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode != "" {
				var body httpx.ErrorBody
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("response is not an error body: %v", err)
				}
				if body.Code != tt.wantCode {
					t.Fatalf("error code = %q, want %q", body.Code, tt.wantCode)
				}
			}
		})
	}
}

// TestFullChain runs Authorize + RequireRole together: the RBAC decision is
// taken on the authenticated principal from the session store.
func TestFullChain(t *testing.T) {
	approverSess := &Session{
		TokenHash: HashToken("approver-token"),
		UserID:    "u_approver",
		TenantID:  "t_1a2b3c4d",
		Roles:     []string{"approver"},
		ExpiresAt: timeNowPlus(time.Hour),
	}
	sessions := newMockSessionStore()
	sessions.sessions[approverSess.TokenHash] = approverSess

	handler := Authorize(sessions)(
		RequireRole(RoleApprover)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		),
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/approval-tickets", nil)
	req.Header.Set("Authorization", "Bearer approver-token")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chain status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/audit-logs", nil)
	req.Header.Set("Authorization", "Bearer approver-token")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("chain status for off-matrix path = %d, want 403", rec.Code)
	}
}

func timeNowPlus(d time.Duration) time.Time {
	return time.Now().Add(d)
}
