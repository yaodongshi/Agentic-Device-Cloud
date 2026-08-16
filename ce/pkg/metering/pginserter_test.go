package metering

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

func TestMeteringPGInserter(t *testing.T) {
	ctx := context.Background()
	ev := &UsageEvent{
		TenantID:       "11111111-2222-3333-4444-555555555555",
		Kind:           "TOOL_CALL",
		Source:         "mcp_gateway",
		MetricKey:      "dev-1",
		Value:          1,
		Unit:           "count",
		OccurredAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		IdempotencyKey: "ik-1",
		Meta:           map[string]any{"a": 1},
	}

	t.Run("nil pool", func(t *testing.T) {
		p := &pgInserter{pool: nil}
		if err := p.InsertBatch(ctx, []*UsageEvent{ev}); err == nil {
			t.Fatal("want nil pool error")
		}
	})

	t.Run("ok", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectBatch().ExpectExec(regexp.QuoteMeta(insertSQL)).
			WithArgs(ev.TenantID, ev.Kind, ev.Source, ev.MetricKey, ev.Value, ev.Unit,
				ev.OccurredAt, ev.IdempotencyKey, []byte(`{"a":1}`)).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
		p := &pgInserter{pool: m}
		if err := p.InsertBatch(ctx, []*UsageEvent{ev}); err != nil {
			t.Fatalf("InsertBatch: %v", err)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("empty optionals and nil meta fall back to defaults", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		empty := &UsageEvent{TenantID: "t1", Kind: "TOOL_CALL", Value: 1,
			OccurredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), IdempotencyKey: "ik-2"}
		m.ExpectBatch().ExpectExec(regexp.QuoteMeta(insertSQL)).
			WithArgs("t1", "TOOL_CALL", nil, nil, int64(1), nil,
				empty.OccurredAt, "ik-2", []byte("{}")).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
		p := &pgInserter{pool: m}
		if err := p.InsertBatch(ctx, []*UsageEvent{empty}); err != nil {
			t.Fatalf("InsertBatch: %v", err)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("permanent vs transient classification", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectBatch().ExpectExec(regexp.QuoteMeta(insertSQL)).
			WithArgs(ev.TenantID, ev.Kind, ev.Source, ev.MetricKey, ev.Value, ev.Unit,
				ev.OccurredAt, ev.IdempotencyKey, []byte(`{"a":1}`)).
			WillReturnError(&pgconn.PgError{Code: "23505"})
		p := &pgInserter{pool: m}
		err = p.InsertBatch(ctx, []*UsageEvent{ev})
		if err == nil {
			t.Fatal("want insert error")
		}
		if isRetryable(err) {
			t.Fatalf("constraint violation must be permanent: %v", err)
		}
	})

	t.Run("generic error is retryable", func(t *testing.T) {
		trans := classifyPgErr(errors.New("conn down"))
		if !isRetryable(trans) {
			t.Fatalf("conn error must be retryable: %v", trans)
		}
		if isNilValue(nil) {
			t.Fatal("untyped nil is not a typed nil")
		}
		if !isNilValue(map[string]any(nil)) {
			t.Fatal("nil map must report typed nil")
		}
		if nullIfEmpty("") != nil || nullIfEmpty("x") != "x" {
			t.Fatal("nullIfEmpty broken")
		}
	})
}
