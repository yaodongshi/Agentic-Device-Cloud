package billing

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Statement statuses (design/82 B3.3). GENERATED is the initial state;
// PAID is set by the payment ledger (out of scope for V1.5); OVERDUE is
// the marker the gateway quota soft-limit seam consumes (see
// Service.Overdue — only the state is exposed, the degradation logic
// itself is not implemented in this package).
const (
	StatusGenerated = "GENERATED"
	StatusPaid      = "PAID"
	StatusOverdue   = "OVERDUE"
)

var (
	// ErrStatementNotFound maps to 404 code 16002 in the Admin API.
	ErrStatementNotFound = errors.New("billing: statement not found")
	// ErrStatementExists reports a (tenant, period) duplicate on insert.
	ErrStatementExists = errors.New("billing: statement already exists")
	// ErrInvalidPeriod rejects malformed or out-of-range billing months.
	ErrInvalidPeriod = errors.New("billing: invalid period")
	// ErrAuditNotWired reports a reconciliation attempt without an audit
	// seam (SetAudit).
	ErrAuditNotWired = errors.New("billing: audit counter not wired")
)

// Statement is one persisted monthly bill (billing_statements, migration
// 0002_billing). All monetary amounts are fen.
type Statement struct {
	ID       string
	TenantID string
	Year     int
	Month    int // 1-12

	// Usage aggregates over adc_usage_events (FR-016 three metrics).
	DevicePeak int   // month's max daily-peak device count (doc/04 7.2)
	ToolCalls  int64 // summed TOOL_CALL events — metered but not priced (doc/04 charges no call fee)
	Tokens     int64 // summed TOKEN_USAGE events

	SubscriptionFeeFen int64
	DeviceFeeFen       int64
	TokenFeeFen        int64
	TotalFeeFen        int64

	Status    string
	PaidAt    *time.Time
	Breakdown DeviceBreakdown

	CreatedAt time.Time
	UpdatedAt time.Time
}

// PeriodLabel renders the statement month as YYYY-MM.
func (s *Statement) PeriodLabel() string {
	return fmt.Sprintf("%04d-%02d", s.Year, s.Month)
}

// Reconciliation asserts the tool-call meter against the audit log for
// one month (FR-016 acceptance: metered detail must reconcile with the
// audit trail). Only tool calls have an audit counterpart: device daily
// peaks come from gateway heartbeats and tokens from the LLM gateway —
// both single-writer feeds with no audit-log mirror.
type Reconciliation struct {
	Period         string `json:"period"`
	ToolCallsUsage int64  `json:"tool_calls_usage"`
	ToolCallsAudit int64  `json:"tool_calls_audit"`
	Matched        bool   `json:"matched"`
}

// Service is the application seam the Admin API drives (design/82 B3.2):
// it pairs the pricing Engine with statement persistence and wires the
// idempotent generate flow plus the audit reconciliation assertion.
type Service struct {
	engine *Engine
	repo   BillRepo
	audit  AuditCounter
	now    func() time.Time
}

// NewService builds a billing service; the audit seam is optional and is
// enabled via SetAudit (reconciliation is advisory).
func NewService(engine *Engine, repo BillRepo) *Service {
	return &Service{engine: engine, repo: repo, now: time.Now}
}

// SetAudit wires the audit-log counter for Reconcile.
func (s *Service) SetAudit(a AuditCounter) { s.audit = a }

// Generate builds and persists the monthly statement for
// (tenant, year, month). It is idempotent: a pre-existing statement for
// the same (tenant, month) is returned with created=false. The
// (tenant, period) unique index makes the check-and-insert race-safe — a
// losing concurrent insert falls back to reading the winner's row.
func (s *Service) Generate(ctx context.Context, tenantID string, year, month int) (*Statement, bool, error) {
	if !ValidPeriod(year, month) {
		return nil, false, ErrInvalidPeriod
	}
	if existing, err := s.repo.FindByPeriod(ctx, tenantID, year, month); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrStatementNotFound) {
		return nil, false, err
	}
	st, err := s.engine.Build(ctx, tenantID, year, month)
	if err != nil {
		return nil, false, err
	}
	st.Status = StatusGenerated
	now := s.now().UTC()
	st.CreatedAt = now
	st.UpdatedAt = now
	created, err := s.repo.InsertStatement(ctx, st)
	if errors.Is(err, ErrStatementExists) {
		existing, findErr := s.repo.FindByPeriod(ctx, tenantID, year, month)
		if findErr != nil {
			return nil, false, findErr
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return created, true, nil
}

// ListStatements pages a tenant's statements, newest period first.
func (s *Service) ListStatements(ctx context.Context, tenantID string, p Page) ([]*Statement, int, error) {
	return s.repo.ListStatements(ctx, tenantID, p)
}

// GetStatement loads one statement by id.
func (s *Service) GetStatement(ctx context.Context, statementID string) (*Statement, error) {
	return s.repo.GetStatement(ctx, statementID)
}

// Reconcile compares the period's metered tool calls against the audit
// log's tool_call rows (FR-016 acceptance). The two sources may trail
// each other by the metering pipeline latency (design/32 1.1: up to 5
// minutes), so a mismatch is advisory, not a billing error.
func (s *Service) Reconcile(ctx context.Context, tenantID string, year, month int) (*Reconciliation, error) {
	if s.audit == nil {
		return nil, ErrAuditNotWired
	}
	from, to := MonthRange(year, month, Shanghai())
	usage, err := s.engine.agg.Aggregate(ctx, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	var calls int64
	if usage != nil {
		calls = usage.ToolCalls
	}
	audit, err := s.audit.CountToolCalls(ctx, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	return &Reconciliation{
		Period:         fmt.Sprintf("%04d-%02d", year, month),
		ToolCallsUsage: calls,
		ToolCallsAudit: audit,
		Matched:        calls == audit,
	}, nil
}

// Overdue reports whether the tenant holds any OVERDUE statement. This
// is the seam the gateway consumes for the B3.3 quota soft-limit (soft
// limit -> read-only degradation); only the state is exposed here — the
// gateway degradation logic itself is intentionally not implemented in
// this package.
func (s *Service) Overdue(ctx context.Context, tenantID string) (bool, error) {
	return s.repo.HasOverdue(ctx, tenantID)
}
