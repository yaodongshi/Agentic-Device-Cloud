package adminapi

import (
	"context"
	"net/http"
	"sync"
	"testing"
)

type memA2ATaskDecisionRepo struct {
	mu      sync.Mutex
	tenant  string
	taskID  string
	state   string
	version int64
	userID  string
	reason  string
}

func (m *memA2ATaskDecisionRepo) Decide(_ context.Context, tenantID, taskID string, expectedVersion int64, decision, userID, reason string) (*A2ATaskDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if tenantID != m.tenant || taskID != m.taskID {
		return nil, ErrA2ATaskNotFound
	}
	if m.state != "input-required" || m.version != expectedVersion {
		return nil, ErrA2ATaskConflict
	}
	if decision == "approve" {
		m.state = "working"
	} else {
		m.state = "rejected"
	}
	m.version++
	m.userID, m.reason = userID, reason
	return &A2ATaskDecision{TaskID: taskID, TenantID: tenantID, State: m.state, Version: m.version,
		ApproverUserID: userID, ApproverIdentity: "Test Approver", DecisionReason: reason}, nil
}

func TestA2ATaskDecisionRoleAndMachineBoundary(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "a2a-role", "A2A Role", nil)
	taskID := uuidOf(700001)
	env.srv.A2ATasks = &memA2ATaskDecisionRepo{tenant: tenant.TenantID, taskID: taskID, state: "input-required", version: 1}
	env.handler = env.srv.Handler()
	path := "/v1/admin/a2a/tasks/" + taskID + "/decision"
	body := map[string]any{"decision": "approve", "expected_version": 1, "reason": "checked"}

	if got := env.do(http.MethodPost, path, "", body).Code; got != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d, want 401", got)
	}
	if got := env.do(http.MethodPost, path, env.token(t, []string{"auditor"}, tenant.TenantID), body).Code; got != http.StatusForbidden {
		t.Fatalf("auditor status=%d, want 403", got)
	}
	if got := env.do(http.MethodPost, path, env.token(t, []string{"approver"}, tenant.TenantID), body).Code; got != http.StatusOK {
		t.Fatalf("approver status=%d, want 200", got)
	}
}

func TestA2ATaskDecisionCASAndTenantIsolation(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "a2a-cas", "A2A CAS", nil)
	other := env.createTenant(t, "a2a-other", "A2A Other", nil)
	taskID := uuidOf(700002)
	repo := &memA2ATaskDecisionRepo{tenant: tenant.TenantID, taskID: taskID, state: "input-required", version: 1}
	env.srv.A2ATasks = repo
	env.handler = env.srv.Handler()
	path := "/v1/admin/a2a/tasks/" + taskID + "/decision"
	body := map[string]any{"decision": "approve", "expected_version": 1, "reason": "safe"}

	if got := env.do(http.MethodPost, path, env.token(t, []string{"approver"}, other.TenantID), body).Code; got != http.StatusNotFound {
		t.Fatalf("cross-tenant status=%d, want 404", got)
	}
	tokens := []string{
		env.token(t, []string{"approver"}, tenant.TenantID),
		env.token(t, []string{"approver"}, tenant.TenantID),
	}
	statuses := make(chan int, 2)
	for _, token := range tokens {
		go func() { statuses <- env.do(http.MethodPost, path, token, body).Code }()
	}
	counts := map[int]int{}
	for range 2 {
		counts[<-statuses]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("statuses=%v, want one 200 and one 409", counts)
	}
	if repo.state != "working" || repo.version != 2 || repo.userID == "" || repo.reason != "safe" {
		t.Fatalf("decision audit/state=%+v", repo)
	}
}
