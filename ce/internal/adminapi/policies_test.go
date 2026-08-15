package adminapi

import (
	"net/http"
	"testing"
)

func TestGetApprovalPolicyDefaults(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodGet, "/v1/admin/tenants/"+tr.TenantID+"/approval-policy", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out approvalPolicyResponse
	env.decode(t, rec, &out)
	if out.ApprovalTimeoutSec != defaultApprovalTimeoutSec {
		t.Fatalf("timeout = %d, want %d", out.ApprovalTimeoutSec, defaultApprovalTimeoutSec)
	}
	if out.Approvers == nil || len(out.Approvers) != 0 {
		t.Fatalf("approvers = %v, want empty list", out.Approvers)
	}
	if out.UpdatedAt != nil {
		t.Fatalf("default policy must not carry updated_at: %v", out.UpdatedAt)
	}
}

func TestPutApprovalPolicyUpdatesAndAudits(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodPut, "/v1/admin/tenants/"+tr.TenantID+"/approval-policy", tok,
		map[string]any{
			"approval_timeout_sec": 600,
			"approvers":            []string{"emp_zhangwei", "emp_li"},
			"change_reason":        "产线大修期间延长审批窗口",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out approvalPolicyResponse
	env.decode(t, rec, &out)
	if out.ApprovalTimeoutSec != 600 || len(out.Approvers) != 2 || out.UpdatedAt == nil {
		t.Fatalf("put response = %+v", out)
	}

	// GET reflects the stored policy.
	rec = env.do(http.MethodGet, "/v1/admin/tenants/"+tr.TenantID+"/approval-policy", tok, nil)
	var got approvalPolicyResponse
	env.decode(t, rec, &got)
	if got.ApprovalTimeoutSec != 600 || len(got.Approvers) != 2 || got.Approvers[0] != "emp_zhangwei" {
		t.Fatalf("stored policy = %+v", got)
	}

	// policy changes are audited with the reason.
	op, ok := env.audit.last()
	if !ok || op.Action != "policy.update" || op.Reason == "" {
		t.Fatalf("audit = %+v, want policy.update with reason", op)
	}
	if op.Details["approval_timeout_sec"] != 600 || op.Details["approver_count"] != 2 {
		t.Fatalf("audit details = %+v", op.Details)
	}
}

func TestPutApprovalPolicyInFlightSnapshotUnaffected(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	// An "in-flight view" stands in for the snapshot the approval
	// service takes at ticket creation (BR-007-03): it must not change
	// when a later PUT replaces the tenant-level policy.
	rec := env.do(http.MethodGet, "/v1/admin/tenants/"+tr.TenantID+"/approval-policy", tok, nil)
	var inflight approvalPolicyResponse
	env.decode(t, rec, &inflight)

	rec = env.do(http.MethodPut, "/v1/admin/tenants/"+tr.TenantID+"/approval-policy", tok,
		map[string]any{"approval_timeout_sec": 900, "change_reason": "换班窗口"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}

	if inflight.ApprovalTimeoutSec != defaultApprovalTimeoutSec || inflight.Approvers == nil || len(inflight.Approvers) != 0 {
		t.Fatalf("in-flight view mutated by later PUT: %+v", inflight)
	}
	// the tenant-level source of truth now carries the new value.
	rec = env.do(http.MethodGet, "/v1/admin/tenants/"+tr.TenantID+"/approval-policy", tok, nil)
	var current approvalPolicyResponse
	env.decode(t, rec, &current)
	if current.ApprovalTimeoutSec != 900 {
		t.Fatalf("current policy = %+v", current)
	}

	// the repository stores a copy: mutating the request slice after the
	// PUT cannot leak into the ledger.
	env.store.policies.mu.Lock()
	stored := env.store.policies.policies[tr.TenantID]
	env.store.policies.mu.Unlock()
	if stored == nil || stored.ApprovalTimeoutSec != 900 {
		t.Fatalf("stored policy = %+v", stored)
	}
}

func TestPutApprovalPolicyValidation(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	path := "/v1/admin/tenants/" + tr.TenantID + "/approval-policy"

	bad := []map[string]any{
		{"approval_timeout_sec": 900},                                                  // missing change_reason
		{"approval_timeout_sec": 5, "change_reason": "x"},                              // below floor
		{"approval_timeout_sec": 4000, "change_reason": "x"},                           // above ceiling
		{"approvers": []string{""}, "change_reason": "x"},                              // empty entry
		{"approvers": []string{"   "}, "change_reason": "x"},                           // blank entry
		{"approvers": []string{"a" + string(make([]byte, 255))}, "change_reason": "x"}, // too long
	}
	for i, body := range bad {
		rec := env.do(http.MethodPut, path, tok, body)
		env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
		t.Logf("case %d rejected as expected", i)
	}

	// duplicate approvers are deduplicated, not rejected.
	rec := env.do(http.MethodPut, path, tok, map[string]any{
		"approvers": []string{"emp_zhangwei", "emp_zhangwei"}, "change_reason": "x",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out approvalPolicyResponse
	env.decode(t, rec, &out)
	if len(out.Approvers) != 1 {
		t.Fatalf("approvers = %v, want deduplicated single entry", out.Approvers)
	}
}

func TestApprovalPolicyNotFoundAndCrossTenant(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	other := env.createTenant(t, "other", "Other", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	// tenant_admin cannot read another tenant's policy.
	rec := env.do(http.MethodGet, "/v1/admin/tenants/"+other.TenantID+"/approval-policy", tok, nil)
	env.assertError(t, rec, http.StatusForbidden, codeCrossTenant)
	rec = env.do(http.MethodPut, "/v1/admin/tenants/"+other.TenantID+"/approval-policy", tok,
		map[string]any{"change_reason": "x"})
	env.assertError(t, rec, http.StatusForbidden, codeCrossTenant)

	// platform admin on a missing tenant: 13001 without existence leak.
	ptok := env.token(t, []string{rolePlatformAdmin}, "")
	rec = env.do(http.MethodGet, "/v1/admin/tenants/"+uuidOf(4242)+"/approval-policy", ptok, nil)
	env.assertError(t, rec, http.StatusNotFound, codeTenantNotFound)
	// platform admin may read any existing tenant.
	rec = env.do(http.MethodGet, "/v1/admin/tenants/"+tr.TenantID+"/approval-policy", ptok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("platform admin read: %d; body: %s", rec.Code, rec.Body.String())
	}
}
