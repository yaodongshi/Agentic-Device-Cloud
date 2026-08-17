package adminapi

// FR-016 billing endpoints (design/82 B3.2). Endpoints:
//
//	GET  /v1/admin/billing/statements           list a tenant's monthly statements
//	POST /v1/admin/billing/generate             generate one month's statement (idempotent)
//	GET  /v1/admin/billing/statements/{id}      statement detail + audit reconciliation assertion
//	GET  /v1/admin/billing/overdue              overdue flag for the gateway quota soft-limit (B3.3)
//
// The pricing, aggregation and statement persistence live in
// internal/billing; these handlers stay thin. Statement list/detail/
// overdue are tenant-scoped like the other admin resources (design/33
// 1.2); generation is a platform_admin exclusive because it creates a
// financial record for an arbitrary tenant. The overdue endpoint only
// exposes the state — the gateway degradation logic (soft limit ->
// read-only) is deliberately not implemented here (design/82 B3.3).

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/billing"
	"adc.dev/ce/internal/httpx"
)

// Billing error codes extend the unified business error set (design/33 1.5).
const (
	codeBillingPeriodInvalid     = "16001"
	codeBillingStatementNotFound = "16002"
)

// billingPeriodRe parses the "YYYY-MM" request format.
var billingPeriodRe = regexp.MustCompile(`^(\d{4})-(\d{2})$`)

// parseBillingPeriod validates a "YYYY-MM" string into year/month.
func parseBillingPeriod(s string) (int, int, error) {
	m := billingPeriodRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, errors.New("period must be YYYY-MM")
	}
	year, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	if !billing.ValidPeriod(year, month) {
		return 0, 0, errors.New("period must be YYYY-MM")
	}
	return year, month, nil
}

// billingStatementResponse mirrors one billing.Statement on the wire.
// Amounts stay fen integers; the console renders yuan (1 yuan = 100 fen).
type billingStatementResponse struct {
	ID                 string                  `json:"id"`
	TenantID           string                  `json:"tenant_id"`
	Period             string                  `json:"period"`
	DevicePeak         int                     `json:"device_peak"`
	ToolCalls          int64                   `json:"tool_calls"`
	Tokens             int64                   `json:"tokens"`
	SubscriptionFeeFen int64                   `json:"subscription_fee_fen"`
	DeviceFeeFen       int64                   `json:"device_fee_fen"`
	TokenFeeFen        int64                   `json:"token_fee_fen"`
	TotalFeeFen        int64                   `json:"total_fee_fen"`
	Status             string                  `json:"status"`
	PaidAt             *time.Time              `json:"paid_at,omitempty"`
	Breakdown          billing.DeviceBreakdown `json:"breakdown"`
	CreatedAt          time.Time               `json:"created_at"`
	UpdatedAt          time.Time               `json:"updated_at"`
}

func newBillingStatementResponse(st *billing.Statement) billingStatementResponse {
	return billingStatementResponse{
		ID:                 st.ID,
		TenantID:           st.TenantID,
		Period:             st.PeriodLabel(),
		DevicePeak:         st.DevicePeak,
		ToolCalls:          st.ToolCalls,
		Tokens:             st.Tokens,
		SubscriptionFeeFen: st.SubscriptionFeeFen,
		DeviceFeeFen:       st.DeviceFeeFen,
		TokenFeeFen:        st.TokenFeeFen,
		TotalFeeFen:        st.TotalFeeFen,
		Status:             st.Status,
		PaidAt:             st.PaidAt,
		Breakdown:          st.Breakdown,
		CreatedAt:          st.CreatedAt.UTC(),
		UpdatedAt:          st.UpdatedAt.UTC(),
	}
}

type billingListResponse struct {
	Items    []billingStatementResponse `json:"items"`
	Total    int                        `json:"total"`
	Page     int                        `json:"page"`
	PageSize int                        `json:"page_size"`
}

// billingReconciliationResponse carries the FR-016 audit assertion: the
// metered tool-call count vs the audit-log tool_call count for the same
// month. A mismatch is advisory — the metering pipeline may trail the
// audit trail by up to 5 minutes (design/32 1.1).
type billingReconciliationResponse struct {
	Period         string `json:"period"`
	ToolCallsUsage int64  `json:"tool_calls_usage"`
	ToolCallsAudit int64  `json:"tool_calls_audit"`
	Matched        bool   `json:"matched"`
}

type billingDetailResponse struct {
	Statement      billingStatementResponse       `json:"statement"`
	Reconciliation *billingReconciliationResponse `json:"reconciliation"`
}

// generateBillingRequest is the POST /v1/admin/billing/generate body.
type generateBillingRequest struct {
	TenantID string `json:"tenant_id"`
	Period   string `json:"period"` // "YYYY-MM"
}

type billingGenerateResponse struct {
	Statement billingStatementResponse `json:"statement"`
	Created   bool                     `json:"created"`
}

// handleBillingList serves GET /v1/admin/billing/statements. Tenant
// scoping follows resolveTenant (design/33 1.2).
func (s *Server) handleBillingList(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	if s.Billing == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "billing service not wired")
		return
	}
	page, err := parsePage(r.URL.Query())
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	list, total, err := s.Billing.ListStatements(r.Context(), tenantID,
		billing.Page{Number: page.Number, Size: page.Size})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "list billing statements failed")
		return
	}
	items := make([]billingStatementResponse, 0, len(list))
	for _, st := range list {
		items = append(items, newBillingStatementResponse(st))
	}
	httpx.WriteJSON(w, http.StatusOK, billingListResponse{
		Items: items, Total: total, Page: page.Number, PageSize: page.Size,
	})
}

// handleBillingGenerate serves POST /v1/admin/billing/generate. It is a
// platform_admin exclusive (financial record for an arbitrary tenant) and
// is idempotent: repeating the same (tenant, month) returns the existing
// statement with created=false instead of a new row. Periods in the
// future are rejected (no usage exists yet).
func (s *Server) handleBillingGenerate(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	if !isPlatformAdmin(p) {
		writeError(w, r, http.StatusForbidden, codeForbidden, "bill generation requires the platform_admin role")
		return
	}
	if s.Billing == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "billing service not wired")
		return
	}
	var req generateBillingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !requireUUID(w, r, "tenant_id", req.TenantID) {
		return
	}
	year, month, err := parseBillingPeriod(req.Period)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBillingPeriodInvalid, err.Error())
		return
	}
	now := s.now().In(billing.Shanghai())
	if year > now.Year() || (year == now.Year() && month > int(now.Month())) {
		writeError(w, r, http.StatusBadRequest, codeBillingPeriodInvalid, "period must not be in the future")
		return
	}
	st, created, err := s.Billing.Generate(r.Context(), req.TenantID, year, month)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "generate billing statement failed")
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: req.TenantID,
		ActorID:  p.UserID,
		Action:   "billing.generate",
		Target:   st.ID,
		Details: map[string]any{
			"period":  st.PeriodLabel(),
			"created": created,
		},
		TraceID: httpx.TraceIDFrom(r),
	})
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, billingGenerateResponse{
		Statement: newBillingStatementResponse(st),
		Created:   created,
	})
}

// handleBillingDetail serves GET /v1/admin/billing/statements/{id}: the
// statement plus the audit reconciliation assertion (FR-016 acceptance).
// Reconciliation is best-effort — a failing audit query must not hide the
// statement, so it degrades to a null reconciliation field.
func (s *Server) handleBillingDetail(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	statementID := r.PathValue("statementID")
	if !requireUUID(w, r, "statement_id", statementID) {
		return
	}
	if s.Billing == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "billing service not wired")
		return
	}
	st, err := s.Billing.GetStatement(r.Context(), statementID)
	if err != nil {
		if errors.Is(err, billing.ErrStatementNotFound) {
			writeError(w, r, http.StatusNotFound, codeBillingStatementNotFound, "billing statement not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load billing statement failed")
		return
	}
	if !s.checkOwnership(w, r, p, st.TenantID) {
		return
	}
	var rec *billingReconciliationResponse
	recResult, recErr := s.Billing.Reconcile(r.Context(), st.TenantID, st.Year, st.Month)
	if recErr == nil && recResult != nil {
		rec = &billingReconciliationResponse{
			Period:         recResult.Period,
			ToolCallsUsage: recResult.ToolCallsUsage,
			ToolCallsAudit: recResult.ToolCallsAudit,
			Matched:        recResult.Matched,
		}
	}
	httpx.WriteJSON(w, http.StatusOK, billingDetailResponse{
		Statement:      newBillingStatementResponse(st),
		Reconciliation: rec,
	})
}

// handleBillingOverdue serves GET /v1/admin/billing/overdue. This is the
// B3.3 quota-linkage seam: the gateway can poll this flag to apply the
// soft limit (read-only degradation) once billing marks a tenant
// OVERDUE. Only the state is exposed — the degradation logic itself is
// intentionally not implemented.
func (s *Server) handleBillingOverdue(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	if s.Billing == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "billing service not wired")
		return
	}
	overdue, err := s.Billing.Overdue(r.Context(), tenantID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load billing overdue status failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"overdue": overdue})
}
