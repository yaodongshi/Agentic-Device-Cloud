package billing

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

const (
	testBillID  = "11111111-2222-3333-4444-555555555555"
	testTenantB = "99999999-8888-7777-6666-555555555555"
)

func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

var billCols = []string{
	"id", "tenant_id", "period_year", "period_month",
	"device_peak", "tool_calls", "tokens",
	"subscription_fee_fen", "device_fee_fen", "token_fee_fen", "total_fee_fen",
	"status", "paid_at", "breakdown", "created_at", "updated_at",
}

func billRowVals(paidAt any) []any {
	return []any{
		testBillID, testTenantB, int(2026), int(8), int(100), int64(5000), int64(8000000),
		int64(331667), int64(266667), int64(80000), int64(678334),
		StatusGenerated, paidAt,
		[]byte(`{"included_devices":20,"tier_400_devices":80,"tier_300_devices":0,"tier_250_devices":0,"annual_device_fee_fen":3200000,"monthly_device_fee_fen":266667}`),
		time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC), time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC),
	}
}

func TestPGAggregatorAggregate(t *testing.T) {
	m := newMockPool(t)
	agg := NewPGAggregator(m)
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	m.ExpectQuery(regexp.QuoteMeta(aggregateSQL)).
		WithArgs(testTenantB, from, to).
		WillReturnRows(pgxmock.NewRows([]string{"event_type", "agg"}).
			AddRow("DEVICE_DAILY_PEAK", int64(45)).
			AddRow("TOOL_CALL", int64(1200)).
			AddRow("TOKEN_USAGE", int64(95000)))
	sum, err := agg.Aggregate(context.Background(), testTenantB, from, to)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if sum.DevicePeak != 45 || sum.ToolCalls != 1200 || sum.Tokens != 95000 {
		t.Fatalf("summary = %+v, want 45/1200/95000", sum)
	}

	// A month with no events aggregates to an all-zero summary.
	m.ExpectQuery(regexp.QuoteMeta(aggregateSQL)).
		WithArgs(testTenantB, from, to).
		WillReturnRows(pgxmock.NewRows([]string{"event_type", "agg"}))
	sum, err = agg.Aggregate(context.Background(), testTenantB, from, to)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if sum.DevicePeak != 0 || sum.ToolCalls != 0 || sum.Tokens != 0 {
		t.Fatalf("summary = %+v, want all zero", sum)
	}

	m.ExpectQuery(regexp.QuoteMeta(aggregateSQL)).
		WithArgs(testTenantB, from, to).
		WillReturnError(errors.New("conn down"))
	if _, err := agg.Aggregate(context.Background(), testTenantB, from, to); err == nil {
		t.Fatal("want query error, got nil")
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGAuditCounterCountToolCalls(t *testing.T) {
	m := newMockPool(t)
	c := NewPGAuditCounter(m)
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	m.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM adc_audit_logs
		WHERE tenant_id = $1::uuid AND event_type = 'tool_call'
		  AND created_at >= $2 AND created_at < $3`)).
		WithArgs(testTenantB, from, to).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(42)))
	n, err := c.CountToolCalls(context.Background(), testTenantB, from, to)
	if err != nil {
		t.Fatalf("CountToolCalls: %v", err)
	}
	if n != 42 {
		t.Fatalf("count = %d, want 42", n)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGBillRepoInsertStatement(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGBillRepo(m)

	stmt := &Statement{
		TenantID:           testTenantB,
		Year:               2026,
		Month:              8,
		DevicePeak:         100,
		ToolCalls:          5000,
		Tokens:             8000000,
		SubscriptionFeeFen: 331667,
		DeviceFeeFen:       266667,
		TokenFeeFen:        80000,
		TotalFeeFen:        678334,
		Status:             StatusGenerated,
		Breakdown:          deviceLadder(100),
	}
	m.ExpectQuery(regexp.QuoteMeta(`INSERT INTO billing_statements
		(tenant_id, period_year, period_month, device_peak, tool_calls, tokens,
		 subscription_fee_fen, device_fee_fen, token_fee_fen, total_fee_fen,
		 status, breakdown)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (tenant_id, period_year, period_month) DO NOTHING
		RETURNING `+statementCols)).
		WithArgs(testTenantB, 2026, 8, 100, int64(5000), int64(8000000),
			int64(331667), int64(266667), int64(80000), int64(678334),
			StatusGenerated,
			`{"included_devices":20,"tier_400_devices":80,"tier_300_devices":0,"tier_250_devices":0,"annual_device_fee_fen":3200000,"monthly_device_fee_fen":266667}`).
		WillReturnRows(pgxmock.NewRows(billCols).AddRow(billRowVals(nil)...))
	created, err := repo.InsertStatement(context.Background(), stmt)
	if err != nil {
		t.Fatalf("InsertStatement: %v", err)
	}
	if created.ID != testBillID || created.DeviceFeeFen != 266667 || created.Breakdown.Tier400Devices != 80 {
		t.Fatalf("created = %+v, want id %s with tier-400 80", created, testBillID)
	}

	// Duplicate (tenant, period): the DO NOTHING conflict yields no row.
	m.ExpectQuery(regexp.QuoteMeta(`INSERT INTO billing_statements
		(tenant_id, period_year, period_month, device_peak, tool_calls, tokens,
		 subscription_fee_fen, device_fee_fen, token_fee_fen, total_fee_fen,
		 status, breakdown)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (tenant_id, period_year, period_month) DO NOTHING
		RETURNING `+statementCols)).
		WithArgs(testTenantB, 2026, 8, 100, int64(5000), int64(8000000),
			int64(331667), int64(266667), int64(80000), int64(678334),
			StatusGenerated,
			`{"included_devices":20,"tier_400_devices":80,"tier_300_devices":0,"tier_250_devices":0,"annual_device_fee_fen":3200000,"monthly_device_fee_fen":266667}`).
		WillReturnError(pgx.ErrNoRows)
	if _, err := repo.InsertStatement(context.Background(), stmt); !errors.Is(err, ErrStatementExists) {
		t.Fatalf("want ErrStatementExists, got %v", err)
	}

	// A raw unique violation maps to the same sentinel.
	m.ExpectQuery(regexp.QuoteMeta(`INSERT INTO billing_statements
		(tenant_id, period_year, period_month, device_peak, tool_calls, tokens,
		 subscription_fee_fen, device_fee_fen, token_fee_fen, total_fee_fen,
		 status, breakdown)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (tenant_id, period_year, period_month) DO NOTHING
		RETURNING `+statementCols)).
		WithArgs(testTenantB, 2026, 8, 100, int64(5000), int64(8000000),
			int64(331667), int64(266667), int64(80000), int64(678334),
			StatusGenerated,
			`{"included_devices":20,"tier_400_devices":80,"tier_300_devices":0,"tier_250_devices":0,"annual_device_fee_fen":3200000,"monthly_device_fee_fen":266667}`).
		WillReturnError(&pgconn.PgError{Code: "23505"})
	if _, err := repo.InsertStatement(context.Background(), stmt); !errors.Is(err, ErrStatementExists) {
		t.Fatalf("want ErrStatementExists for 23505, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGBillRepoListStatements(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGBillRepo(m)

	m.ExpectQuery(regexp.QuoteMeta(`SELECT `+statementCols+`, count(*) OVER () AS total
		FROM billing_statements
		WHERE tenant_id = $1::uuid
		ORDER BY period_year DESC, period_month DESC, id
		LIMIT $2 OFFSET $3`)).
		WithArgs(testTenantB, 20, 0).
		WillReturnRows(pgxmock.NewRows(append(billCols, "total")).
			AddRow(append(billRowVals(nil), int(1))...))
	list, total, err := repo.ListStatements(context.Background(), testTenantB, Page{Number: 1, Size: 20})
	if err != nil {
		t.Fatalf("ListStatements: %v", err)
	}
	if total != 1 || len(list) != 1 || list[0].ID != testBillID {
		t.Fatalf("list = %d/%+v, want total 1 with id %s", total, list, testBillID)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGBillRepoGetStatement(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGBillRepo(m)

	q := regexp.QuoteMeta(`SELECT ` + statementCols + ` FROM billing_statements
		WHERE id = $1::uuid`)
	m.ExpectQuery(q).
		WithArgs(testBillID).
		WillReturnRows(pgxmock.NewRows(billCols).AddRow(billRowVals(nil)...))
	st, err := repo.GetStatement(context.Background(), testBillID)
	if err != nil {
		t.Fatalf("GetStatement: %v", err)
	}
	if st.ID != testBillID || st.Status != StatusGenerated {
		t.Fatalf("statement = %+v", st)
	}

	m.ExpectQuery(q).
		WithArgs(testBillID).
		WillReturnError(pgx.ErrNoRows)
	if _, err := repo.GetStatement(context.Background(), testBillID); !errors.Is(err, ErrStatementNotFound) {
		t.Fatalf("want ErrStatementNotFound, got %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGBillRepoFindByPeriod(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGBillRepo(m)

	q := regexp.QuoteMeta(`SELECT ` + statementCols + ` FROM billing_statements
		WHERE tenant_id = $1::uuid AND period_year = $2 AND period_month = $3`)
	m.ExpectQuery(q).
		WithArgs(testTenantB, 2026, 8).
		WillReturnRows(pgxmock.NewRows(billCols).AddRow(billRowVals(nil)...))
	st, err := repo.FindByPeriod(context.Background(), testTenantB, 2026, 8)
	if err != nil {
		t.Fatalf("FindByPeriod: %v", err)
	}
	if st.Year != 2026 || st.Month != 8 {
		t.Fatalf("statement = %+v, want 2026-08", st)
	}

	m.ExpectQuery(q).
		WithArgs(testTenantB, 2026, 7).
		WillReturnError(pgx.ErrNoRows)
	if _, err := repo.FindByPeriod(context.Background(), testTenantB, 2026, 7); !errors.Is(err, ErrStatementNotFound) {
		t.Fatalf("want ErrStatementNotFound, got %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGBillRepoHasOverdue(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGBillRepo(m)

	q := regexp.QuoteMeta(`SELECT EXISTS(
		SELECT 1 FROM billing_statements
		WHERE tenant_id = $1::uuid AND status = 'OVERDUE')`)
	m.ExpectQuery(q).
		WithArgs(testTenantB).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
	overdue, err := repo.HasOverdue(context.Background(), testTenantB)
	if err != nil {
		t.Fatalf("HasOverdue: %v", err)
	}
	if !overdue {
		t.Fatal("want overdue true")
	}

	m.ExpectQuery(q).
		WithArgs(testTenantB).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	overdue, err = repo.HasOverdue(context.Background(), testTenantB)
	if err != nil {
		t.Fatalf("HasOverdue: %v", err)
	}
	if overdue {
		t.Fatal("want overdue false")
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
