package adminapi

// C5.1 tenant budget alerting (design/83): a monthly budget line
// (metadata.budget_monthly_cents, doc/04 budget discipline) versus the
// current month's metered fee, priced by the billing engine without
// persisting a statement. Status semantics follow LiteLLM budgets:
//
//	OK       usage below 80%
//	WARN     usage >= 80%
//	EXCEEDED usage >= 100%
//
// Endpoints:
//
//	GET /v1/admin/tenants/{tenantID}/budget-status  tenant-scoped read
//	PUT /v1/admin/tenants/{tenantID}/budget         platform_admin write
//
// The budget line lives in adc_tenants.metadata (JSONB merge via the
// TenantRepo metadata seam, policies.go discipline); a zero line removes
// the key and disables the budget. The fee source is the optional
// BudgetUsageRepo seam — nil fails the GET closed with 500 code 10007
// until the assembly wires it (the PG implementation prices the current
// month through billing.NewEngine over adc_usage_events).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/billing"
	"adc.dev/ce/internal/httpx"
)

// budgetMetaKey is the adc_tenants.metadata JSONB key holding the monthly
// budget line in fen (1 yuan = 100 fen, same unit as billing fees).
const budgetMetaKey = "budget_monthly_cents"

// Budget status values (design/83 C5.1): WARN at 80% usage, EXCEEDED at
// 100%.
const (
	budgetStatusOK       = "OK"
	budgetStatusWarn     = "WARN"
	budgetStatusExceeded = "EXCEEDED"

	budgetWarnPercent     = 80
	budgetExceededPercent = 100
)

// BudgetUsageRepo prices the current month's metered usage for one
// tenant through the billing pricing engine (design/82 B3.1, doc/04
// fee formula) without persisting a statement.
type BudgetUsageRepo interface {
	CurrentMonthFeeFen(ctx context.Context, tenantID string) (int64, error)
}

// budgetPercent computes the floored integer usage percentage; a zero
// budget line means "no budget" and reports 0%.
func budgetPercent(billedFen, budgetCents int64) int {
	if budgetCents <= 0 {
		return 0
	}
	return int(billedFen * 100 / budgetCents)
}

// budgetStatus maps a usage percentage onto the C5.1 thresholds.
func budgetStatus(percent int) string {
	if percent >= budgetExceededPercent {
		return budgetStatusExceeded
	}
	if percent >= budgetWarnPercent {
		return budgetStatusWarn
	}
	return budgetStatusOK
}

// metaInt64 reads an integer out of a JSON-decoded metadata map.
// json.Unmarshal yields float64 for numbers, json.Number when the caller
// decodes with UseNumber, and the in-memory repositories keep native
// integers; all three spellings are accepted. Absent or malformed values
// answer 0.
func metaInt64(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case int:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	}
	return 0
}

// budgetStatusResponse is the GET wire shape (design/83 C5.1). Amounts
// stay integer fen; the console renders yuan.
type budgetStatusResponse struct {
	TenantID           string `json:"tenant_id"`
	Period             string `json:"period"`
	BudgetMonthlyCents int64  `json:"budget_monthly_cents"`
	BilledFen          int64  `json:"billed_fen"`
	UsagePercent       int    `json:"usage_percent"`
	Status             string `json:"status"`
}

// budgetLineResponse is the PUT wire shape.
type budgetLineResponse struct {
	TenantID           string `json:"tenant_id"`
	BudgetMonthlyCents int64  `json:"budget_monthly_cents"`
}

// handleGetBudgetStatus serves GET /v1/admin/tenants/{tenantID}/budget-status.
// Tenant scoping follows the approval policy (design/33 1.2): tenant
// admins are locked to their own tenant, platform admins may read any.
func (s *Server) handleGetBudgetStatus(w http.ResponseWriter, r *http.Request) {
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
	if s.BudgetUsage == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "budget usage repository not wired")
		return
	}
	meta, err := s.Tenants.GetMeta(r.Context(), tenantID)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			writeError(w, r, http.StatusNotFound, codeTenantNotFound, "tenant not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load tenant metadata failed")
		return
	}
	budget := metaInt64(meta, budgetMetaKey)
	billed, err := s.BudgetUsage.CurrentMonthFeeFen(r.Context(), tenantID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "aggregate current month usage failed")
		return
	}
	now := s.now().In(billing.Shanghai())
	percent := budgetPercent(billed, budget)
	httpx.WriteJSON(w, http.StatusOK, budgetStatusResponse{
		TenantID:           tenantID,
		Period:             fmt.Sprintf("%04d-%02d", now.Year(), int(now.Month())),
		BudgetMonthlyCents: budget,
		BilledFen:          billed,
		UsagePercent:       percent,
		Status:             budgetStatus(percent),
	})
}

// putBudgetRequest is the PUT body: the new monthly budget line in fen
// (0 removes the line) plus the mandatory audit reason.
type putBudgetRequest struct {
	BudgetMonthlyCents int64  `json:"budget_monthly_cents"`
	ChangeReason       string `json:"change_reason"`
}

// handlePutBudget serves PUT /v1/admin/tenants/{tenantID}/budget. The
// budget line is a financial configuration, so the write is a
// platform_admin exclusive like billing generation (design/82 B3.2).
func (s *Server) handlePutBudget(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	if !isPlatformAdmin(p) {
		writeError(w, r, http.StatusForbidden, codeForbidden, "budget updates require the platform_admin role")
		return
	}
	tenantID := r.PathValue("tenantID")
	if !requireUUID(w, r, "tenant_id", tenantID) {
		return
	}
	var req putBudgetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ChangeReason) == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "change_reason is required")
		return
	}
	if req.BudgetMonthlyCents < 0 {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "budget_monthly_cents must not be negative")
		return
	}
	// A zero line removes the metadata key (SetMeta nil-value semantics),
	// disabling the budget; a positive line overwrites it.
	kv := map[string]any{budgetMetaKey: req.BudgetMonthlyCents}
	if req.BudgetMonthlyCents == 0 {
		kv[budgetMetaKey] = nil
	}
	meta, err := s.Tenants.SetMeta(r.Context(), tenantID, kv)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			writeError(w, r, http.StatusNotFound, codeTenantNotFound, "tenant not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "update budget line failed")
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: tenantID,
		ActorID:  p.UserID,
		Action:   "budget.update",
		Target:   tenantID,
		Reason:   req.ChangeReason,
		Details: map[string]any{
			"budget_monthly_cents": req.BudgetMonthlyCents,
		},
		TraceID: httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusOK, budgetLineResponse{
		TenantID:           tenantID,
		BudgetMonthlyCents: metaInt64(meta, budgetMetaKey),
	})
}

// ---------------------------------------------------------------------------
// PostgreSQL budget usage repository
// ---------------------------------------------------------------------------

// pgBudgetUsageRepo prices the current month's usage through the billing
// engine over adc_usage_events (design/82 B3.1); unlike
// billing.Service.Generate nothing is persisted.
type pgBudgetUsageRepo struct {
	eng *billing.Engine
	now func() time.Time
}

// NewPGBudgetUsageRepo builds a budget usage repo over an existing pgx
// pool (billing.PG).
func NewPGBudgetUsageRepo(pool billing.PG) *pgBudgetUsageRepo {
	return &pgBudgetUsageRepo{
		eng: billing.NewEngine(billing.NewPGAggregator(pool)),
		now: time.Now,
	}
}

// CurrentMonthFeeFen aggregates the tenant's usage since the first of
// the current business month (Asia/Shanghai, doc/04 calendar) and prices
// it with the doc/04 fee formula; the result is the month-to-date bill.
func (r *pgBudgetUsageRepo) CurrentMonthFeeFen(ctx context.Context, tenantID string) (int64, error) {
	now := r.now().In(billing.Shanghai())
	st, err := r.eng.Build(ctx, tenantID, now.Year(), int(now.Month()))
	if err != nil {
		return 0, err
	}
	return st.TotalFeeFen, nil
}
