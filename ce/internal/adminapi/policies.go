package adminapi

// design/80 B-04 second half: approval policy configuration, the V1
// single-level subset of FR-007. Endpoints:
//
//	GET /v1/admin/tenants/{tenantID}/approval-policy
//	PUT /v1/admin/tenants/{tenantID}/approval-policy
//
// Migration 0001 defines no approval-policy table or dedicated columns
// on adc_tenants (design/32 3.1), so V1 carries the policy inside
// adc_tenants.metadata under the "approval_policy" key:
//
//	{"approval_policy": {"approval_timeout_sec": 300, "approvers": [...]}}
//
// A PostgreSQL PolicyRepo implementation MUST merge JSONB keys
// (metadata || $policy), never replace the document, because
// pgTenantRepo already stores quota fields (max_agent_keys,
// audit_retention_days) in the same column (tenants.go Quota).
//
// BR-007-03 in-flight semantics: policy changes only affect NEW
// tickets. The approval service snapshots this policy into each ticket
// at creation time (approver list plus expires_at = created_at +
// approval_timeout_sec); a PUT only replaces the tenant-level source of
// truth, so PENDING tickets created earlier keep their snapshot until
// they resolve. This package has no ticket coupling - the guarantee is
// enforced where tickets are created (approval service), which is why
// only the stored policy object is swapped here, never mutated.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/httpx"
)

// Approval policy bounds (V1 single level, FR-007 / FR-005).
const (
	// defaultApprovalTimeoutSec is the V1 default ticket lifetime
	// (design/33 3.2.3: 5 minutes, FR-005).
	defaultApprovalTimeoutSec = 300
	// minApprovalTimeoutSec / maxApprovalTimeoutSec are the V1 sanity
	// window for the configurable timeout. The upper bound keeps the
	// FR-005 expiry promise observable (a PENDING ticket must not live
	// forever) and stays below the HITL escalation horizon (V1.5).
	minApprovalTimeoutSec = 10
	maxApprovalTimeoutSec = 3600
	// maxApprovers caps the single-level approver roster (FR-007 V1:
	// fixed backup approver only, no escalation chains).
	maxApprovers = 50
)

// ApprovalPolicy is the tenant approval policy (FR-007 V1 single-level
// subset). UpdatedAt is zero while the policy is the implicit default.
type ApprovalPolicy struct {
	TenantID           string
	ApprovalTimeoutSec int
	Approvers          []string
	UpdatedAt          time.Time
}

// defaultPolicy is returned by GetPolicy when nothing is configured
// (BR-006-01 default-2 sibling rule for HITL: default timeout 300s,
// fail-safe empty approver roster - the approval service must refuse
// ticket creation rather than approve nobody, FR-005 fail-safe).
func defaultPolicy(tenantID string) *ApprovalPolicy {
	return &ApprovalPolicy{TenantID: tenantID, ApprovalTimeoutSec: defaultApprovalTimeoutSec, Approvers: []string{}}
}

// PolicyRepo persists the tenant approval policy (design/31 LLD 3.4.2
// toolpolicy seam). GetPolicy returns the stored policy or the V1
// defaults; both methods answer ErrTenantNotFound for unknown tenants.
type PolicyRepo interface {
	GetPolicy(ctx context.Context, tenantID string) (*ApprovalPolicy, error)
	SetPolicy(ctx context.Context, tenantID string, p *ApprovalPolicy) (*ApprovalPolicy, error)
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

type approvalPolicyResponse struct {
	TenantID           string     `json:"tenant_id"`
	ApprovalTimeoutSec int        `json:"approval_timeout_sec"`
	Approvers          []string   `json:"approvers"`
	UpdatedAt          *time.Time `json:"updated_at,omitempty"`
}

func newApprovalPolicyResponse(p *ApprovalPolicy) approvalPolicyResponse {
	resp := approvalPolicyResponse{
		TenantID:           p.TenantID,
		ApprovalTimeoutSec: p.ApprovalTimeoutSec,
		Approvers:          append([]string{}, p.Approvers...),
	}
	if !p.UpdatedAt.IsZero() {
		t := p.UpdatedAt.UTC()
		resp.UpdatedAt = &t
	}
	return resp
}

// putApprovalPolicyRequest mirrors the V1 policy body. PUT is a full
// replace: omitted fields reset to the V1 defaults (timeout 300, empty
// approver roster).
type putApprovalPolicyRequest struct {
	ApprovalTimeoutSec *int     `json:"approval_timeout_sec"`
	Approvers          []string `json:"approvers"`
	ChangeReason       string   `json:"change_reason"`
}

// handleGetApprovalPolicy serves GET /v1/admin/tenants/{tenantID}/approval-policy.
// tenant_admin is locked to its own tenant (13007 otherwise); platform
// admins may read any tenant by path (design/33 1.2).
func (s *Server) handleGetApprovalPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID := r.PathValue("tenantID")
	if !requireUUID(w, r, "tenant_id", tenantID) {
		return
	}
	if !isPlatformAdmin(p) && tenantID != p.TenantID {
		writeError(w, r, http.StatusForbidden, codeCrossTenant, "cross-tenant access denied")
		return
	}
	if s.Policies == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "policy repository not wired")
		return
	}
	pol, err := s.Policies.GetPolicy(r.Context(), tenantID)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			writeError(w, r, http.StatusNotFound, codeTenantNotFound, "tenant not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load policy failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newApprovalPolicyResponse(pol))
}

// handlePutApprovalPolicy serves PUT /v1/admin/tenants/{tenantID}/approval-policy.
// The stored policy feeds new tickets only (BR-007-03); in-flight
// tickets keep their creation-time snapshot (see package comment).
func (s *Server) handlePutApprovalPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID := r.PathValue("tenantID")
	if !requireUUID(w, r, "tenant_id", tenantID) {
		return
	}
	if !isPlatformAdmin(p) && tenantID != p.TenantID {
		writeError(w, r, http.StatusForbidden, codeCrossTenant, "cross-tenant access denied")
		return
	}
	if s.Policies == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "policy repository not wired")
		return
	}
	var req putApprovalPolicyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ChangeReason) == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "change_reason is required")
		return
	}
	pol := defaultPolicy(tenantID)
	if req.ApprovalTimeoutSec != nil {
		if *req.ApprovalTimeoutSec < minApprovalTimeoutSec || *req.ApprovalTimeoutSec > maxApprovalTimeoutSec {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "approval_timeout_sec must be between 10 and 3600")
			return
		}
		pol.ApprovalTimeoutSec = *req.ApprovalTimeoutSec
	}
	seen := make(map[string]bool, len(req.Approvers))
	for _, a := range req.Approvers {
		a = strings.TrimSpace(a)
		if a == "" || len(a) > 255 {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "approvers entries must be 1-255 chars")
			return
		}
		if seen[a] {
			continue
		}
		seen[a] = true
		pol.Approvers = append(pol.Approvers, a)
	}
	if len(pol.Approvers) > maxApprovers {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "too many approvers")
		return
	}
	updated, err := s.Policies.SetPolicy(r.Context(), tenantID, pol)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			writeError(w, r, http.StatusNotFound, codeTenantNotFound, "tenant not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "update policy failed")
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: tenantID,
		ActorID:  p.UserID,
		Action:   "policy.update",
		Target:   tenantID,
		Reason:   req.ChangeReason,
		Details: map[string]any{
			"approval_timeout_sec": updated.ApprovalTimeoutSec,
			"approver_count":       len(updated.Approvers),
		},
		TraceID: httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusOK, newApprovalPolicyResponse(updated))
}
