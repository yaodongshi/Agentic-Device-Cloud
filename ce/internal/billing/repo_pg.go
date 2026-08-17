package billing

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// statementCols rebuilds a Statement; UUID columns are cast to text
// because pgx v5.10 has no binary uuid-to-string scan plan (same approach
// as the adminapi PG repositories).
const statementCols = `id::text, tenant_id::text, period_year, period_month,
	device_peak, tool_calls, tokens,
	subscription_fee_fen, device_fee_fen, token_fee_fen, total_fee_fen,
	status, paid_at, breakdown, created_at, updated_at`

// PGBillRepo is the production BillRepo over billing_statements
// (migration 0002_billing).
type PGBillRepo struct {
	pool PG
}

// NewPGBillRepo builds a statement repository over an existing pgx pool.
func NewPGBillRepo(pool PG) *PGBillRepo { return &PGBillRepo{pool: pool} }

func (r *PGBillRepo) scanStatement(row pgx.Row) (*Statement, error) {
	var (
		st        Statement
		breakdown []byte
	)
	err := row.Scan(&st.ID, &st.TenantID, &st.Year, &st.Month,
		&st.DevicePeak, &st.ToolCalls, &st.Tokens,
		&st.SubscriptionFeeFen, &st.DeviceFeeFen, &st.TokenFeeFen, &st.TotalFeeFen,
		&st.Status, &st.PaidAt, &breakdown, &st.CreatedAt, &st.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if len(breakdown) > 0 {
		if err := json.Unmarshal(breakdown, &st.Breakdown); err != nil {
			return nil, err
		}
	}
	return &st, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// InsertStatement stores a new statement. The ON CONFLICT clause targets
// the (tenant_id, period_year, period_month) unique constraint: a
// duplicate insert yields no row and is reported as ErrStatementExists
// (the Service then re-reads the winner's row).
func (r *PGBillRepo) InsertStatement(ctx context.Context, st *Statement) (*Statement, error) {
	breakdown, err := json.Marshal(st.Breakdown)
	if err != nil {
		return nil, err
	}
	row := r.pool.QueryRow(ctx, `INSERT INTO billing_statements
		(tenant_id, period_year, period_month, device_peak, tool_calls, tokens,
		 subscription_fee_fen, device_fee_fen, token_fee_fen, total_fee_fen,
		 status, breakdown)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (tenant_id, period_year, period_month) DO NOTHING
		RETURNING `+statementCols,
		st.TenantID, st.Year, st.Month, st.DevicePeak, st.ToolCalls, st.Tokens,
		st.SubscriptionFeeFen, st.DeviceFeeFen, st.TokenFeeFen, st.TotalFeeFen,
		st.Status, string(breakdown))
	created, err := r.scanStatement(row)
	if errors.Is(err, pgx.ErrNoRows) || isUniqueViolation(err) {
		return nil, ErrStatementExists
	}
	if err != nil {
		return nil, err
	}
	return created, nil
}

// ListStatements pages a tenant's statements, newest period first.
func (r *PGBillRepo) ListStatements(ctx context.Context, tenantID string, p Page) ([]*Statement, int, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+statementCols+`, count(*) OVER () AS total
		FROM billing_statements
		WHERE tenant_id = $1::uuid
		ORDER BY period_year DESC, period_month DESC, id
		LIMIT $2 OFFSET $3`, tenantID, p.Size, p.offset())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var (
		out   []*Statement
		total int
	)
	for rows.Next() {
		var (
			st        Statement
			breakdown []byte
		)
		// All columns including the window total are scanned in one call:
		// a second Scan on the same Rows would advance to the next row.
		if err := rows.Scan(&st.ID, &st.TenantID, &st.Year, &st.Month,
			&st.DevicePeak, &st.ToolCalls, &st.Tokens,
			&st.SubscriptionFeeFen, &st.DeviceFeeFen, &st.TokenFeeFen, &st.TotalFeeFen,
			&st.Status, &st.PaidAt, &breakdown, &st.CreatedAt, &st.UpdatedAt,
			&total); err != nil {
			return nil, 0, err
		}
		if len(breakdown) > 0 {
			if err := json.Unmarshal(breakdown, &st.Breakdown); err != nil {
				return nil, 0, err
			}
		}
		out = append(out, &st)
	}
	return out, total, rows.Err()
}

// GetStatement loads one statement by id.
func (r *PGBillRepo) GetStatement(ctx context.Context, statementID string) (*Statement, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+statementCols+` FROM billing_statements
		WHERE id = $1::uuid`, statementID)
	st, err := r.scanStatement(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStatementNotFound
	}
	if err != nil {
		return nil, err
	}
	return st, nil
}

// FindByPeriod loads the statement for (tenant, year, month).
func (r *PGBillRepo) FindByPeriod(ctx context.Context, tenantID string, year, month int) (*Statement, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+statementCols+` FROM billing_statements
		WHERE tenant_id = $1::uuid AND period_year = $2 AND period_month = $3`,
		tenantID, year, month)
	st, err := r.scanStatement(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStatementNotFound
	}
	if err != nil {
		return nil, err
	}
	return st, nil
}

// HasOverdue reports any OVERDUE statement for the tenant (B3.3 seam).
func (r *PGBillRepo) HasOverdue(ctx context.Context, tenantID string) (bool, error) {
	var n bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM billing_statements
		WHERE tenant_id = $1::uuid AND status = 'OVERDUE')`, tenantID).Scan(&n)
	return n, err
}

// PGAuditCounter counts audit-log tool_call rows (adc_audit_logs is
// partitioned by created_at; the tenant+time filter rides the partition
// index idx_audit_tenant_time).
type PGAuditCounter struct {
	pool PG
}

// NewPGAuditCounter builds an audit counter over an existing pgx pool.
func NewPGAuditCounter(pool PG) *PGAuditCounter { return &PGAuditCounter{pool: pool} }

// CountToolCalls counts tool_call audit rows in the window. The
// event_type literal is a compile-time constant, not user input (SEC-20).
func (c *PGAuditCounter) CountToolCalls(ctx context.Context, tenantID string, from, to time.Time) (int64, error) {
	var n int64
	err := c.pool.QueryRow(ctx, `SELECT count(*) FROM adc_audit_logs
		WHERE tenant_id = $1::uuid AND event_type = 'tool_call'
		  AND created_at >= $2 AND created_at < $3`, tenantID, from, to).Scan(&n)
	return n, err
}
