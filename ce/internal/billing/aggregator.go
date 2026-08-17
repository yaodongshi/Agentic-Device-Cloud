package billing

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"adc.dev/ce/pkg/metering"
)

// UsageSummary is one aggregation window of adc_usage_events (FR-016,
// design/32 3.11): the maximum of the window's DEVICE_DAILY_PEAK values
// and the sums of TOOL_CALL and TOKEN_USAGE metric values.
type UsageSummary struct {
	DevicePeak int
	ToolCalls  int64
	Tokens     int64
}

// Aggregator windows adc_usage_events into a UsageSummary over the
// half-open [from, to) window. A day-sized window yields the daily usage,
// a month-sized window the monthly usage (design/82 B3.1); see DayRange
// and MonthRange for window construction.
type Aggregator interface {
	Aggregate(ctx context.Context, tenantID string, from, to time.Time) (*UsageSummary, error)
}

// PG is the minimal pgx pool surface the billing PG repositories use.
// *pgxpool.Pool satisfies it in production; unit tests inject pgxmock.
type PG interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PGAggregator is the production Aggregator over pgx.
type PGAggregator struct {
	pool PG
}

// NewPGAggregator builds an aggregator over an existing pgx pool.
func NewPGAggregator(pool PG) *PGAggregator { return &PGAggregator{pool: pool} }

// aggregateSQL groups adc_usage_events by kind for one tenant and window.
// The kind values are compile-time constants (metering.Kind*), never user
// input, so they are inlined; tenant and window travel as parameters
// (SEC-20: no string concatenation of user input).
const aggregateSQL = `
SELECT event_type,
       CASE WHEN event_type = 'DEVICE_DAILY_PEAK'
            THEN MAX(metric_value) ELSE SUM(metric_value) END AS agg
  FROM adc_usage_events
 WHERE tenant_id = $1::uuid AND occurred_at >= $2 AND occurred_at < $3
 GROUP BY event_type`

func (a *PGAggregator) Aggregate(ctx context.Context, tenantID string, from, to time.Time) (*UsageSummary, error) {
	if a == nil || a.pool == nil {
		return nil, errors.New("billing: nil pgx pool")
	}
	rows, err := a.pool.Query(ctx, aggregateSQL, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sum := &UsageSummary{}
	for rows.Next() {
		var (
			kind string
			v    int64
		)
		if err := rows.Scan(&kind, &v); err != nil {
			return nil, err
		}
		switch kind {
		case metering.KindDeviceDaily:
			if int(v) > sum.DevicePeak {
				sum.DevicePeak = int(v)
			}
		case metering.KindCall:
			sum.ToolCalls = v
		case metering.KindToken:
			sum.Tokens = v
		}
	}
	return sum, rows.Err()
}

// Page is the offset pagination window for statement lists (same
// conventions as design/33 1.6; the Admin API converts its own Page).
type Page struct {
	Number int
	Size   int
}

func (p Page) offset() int { return (p.Number - 1) * p.Size }

// BillRepo persists billing_statements rows (migration 0002_billing).
type BillRepo interface {
	// ListStatements pages a tenant's statements, newest period first.
	ListStatements(ctx context.Context, tenantID string, p Page) ([]*Statement, int, error)
	// GetStatement loads one statement by id; ErrStatementNotFound when
	// unknown.
	GetStatement(ctx context.Context, statementID string) (*Statement, error)
	// FindByPeriod loads the statement for (tenant, year, month), or
	// ErrStatementNotFound.
	FindByPeriod(ctx context.Context, tenantID string, year, month int) (*Statement, error)
	// InsertStatement stores a new statement; a (tenant, period)
	// duplicate answers ErrStatementExists.
	InsertStatement(ctx context.Context, st *Statement) (*Statement, error)
	// HasOverdue reports whether the tenant holds any OVERDUE statement
	// (the B3.3 gateway quota soft-limit seam).
	HasOverdue(ctx context.Context, tenantID string) (bool, error)
}

// AuditCounter counts audit-log tool_call rows for the FR-016
// reconciliation assertion (metered calls vs the audit trail).
type AuditCounter interface {
	CountToolCalls(ctx context.Context, tenantID string, from, to time.Time) (int64, error)
}
