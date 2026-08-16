package audit

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

func TestPGInserterInsertBatch(t *testing.T) {
	ctx := context.Background()
	ev := func(eventID string) *AuditEvent {
		return &AuditEvent{
			EventID:   eventID,
			TenantID:  "11111111-2222-3333-4444-555555555555",
			AgentID:   "demo-agent",
			DeviceID:  "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			ToolName:  "get_position",
			Params:    map[string]any{"x": float64(1)},
			Result:    map[string]any{"x": float64(1)},
			Status:    StatusSuccess,
			RiskLevel: 0,
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}
	}

	t.Run("nil pool", func(t *testing.T) {
		p := &pgInserter{pool: nil}
		if err := p.InsertBatch(ctx, []*AuditEvent{ev("e1")}); err == nil {
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
			WithArgs("e1", "11111111-2222-3333-4444-555555555555", EventTypeToolCall, ActorTypeAgent,
				"demo-agent", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "get_position", 0,
				[]byte(`{"x":1}`), []byte(`{"x":1}`), StatusSuccess, nil, "e1",
				time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
		p := &pgInserter{pool: m}
		if err := p.InsertBatch(ctx, []*AuditEvent{ev("e1")}); err != nil {
			t.Fatalf("InsertBatch: %v", err)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("optional fields become NULL", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		empty := &AuditEvent{EventID: "e2", TenantID: "t1", Status: StatusFailed}
		m.ExpectBatch().ExpectExec(regexp.QuoteMeta(insertSQL)).
			WithArgs("e2", "t1", EventTypeToolCall, ActorTypeAgent,
				nil, nil, nil, 0, nil, nil, StatusFailed, nil, "e2", pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
		p := &pgInserter{pool: m}
		if err := p.InsertBatch(ctx, []*AuditEvent{empty}); err != nil {
			t.Fatalf("InsertBatch: %v", err)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("permanent insert error classified", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectBatch().ExpectExec(regexp.QuoteMeta(insertSQL)).
			WithArgs("e3", "11111111-2222-3333-4444-555555555555", EventTypeToolCall, ActorTypeAgent,
				"demo-agent", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "get_position", 0,
				[]byte(`{"x":1}`), []byte(`{"x":1}`), StatusSuccess, nil, "e3",
				time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)).
			WillReturnError(&pgconn.PgError{Code: "23505"})
		p := &pgInserter{pool: m}
		err = p.InsertBatch(ctx, []*AuditEvent{ev("e3")})
		if err == nil {
			t.Fatal("want insert error")
		}
		if isRetryable(err) {
			t.Fatalf("constraint violation must be permanent, got retryable: %v", err)
		}
	})

	t.Run("transient insert error classified", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectBatch().ExpectExec(regexp.QuoteMeta(insertSQL)).
			WithArgs("e4", "11111111-2222-3333-4444-555555555555", EventTypeToolCall, ActorTypeAgent,
				"demo-agent", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "get_position", 0,
				[]byte(`{"x":1}`), []byte(`{"x":1}`), StatusSuccess, nil, "e4",
				time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)).
			WillReturnError(errors.New("conn reset"))
		p := &pgInserter{pool: m}
		err = p.InsertBatch(ctx, []*AuditEvent{ev("e4")})
		if err == nil {
			t.Fatal("want insert error")
		}
		if !isRetryable(err) {
			t.Fatalf("conn error must be retryable, got permanent: %v", err)
		}
	})
}

func TestClassifyPgErrAndHelpers(t *testing.T) {
	perm := classifyPgErr(&pgconn.PgError{Code: "22021"})
	if isRetryable(perm) {
		t.Fatalf("class 22 must be permanent: %v", perm)
	}
	perm = classifyPgErr(&pgconn.PgError{Code: "23P01"})
	if isRetryable(perm) {
		t.Fatalf("class 23 must be permanent: %v", perm)
	}
	trans := classifyPgErr(errors.New("connection refused"))
	if !isRetryable(trans) {
		t.Fatalf("generic error must be retryable: %v", trans)
	}
	if nullIfEmpty("") != nil {
		t.Fatal("empty string must map to nil")
	}
	if nullIfEmpty("x") != "x" {
		t.Fatal("non-empty string must pass through")
	}
	raw, err := marshalJSONB(nil)
	if raw != nil || err != nil {
		t.Fatalf("nil must marshal to SQL NULL: %v %v", raw, err)
	}
	raw, err = marshalJSONB(map[string]any{"a": 1})
	if err != nil || len(raw.([]byte)) == 0 {
		t.Fatalf("map must marshal: %v %v", raw, err)
	}
}
