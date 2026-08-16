package approval

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

const (
	pgUUID  = "11111111-2222-3333-4444-555555555555"
	pgDevID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

var ticketRowCols = []string{
	"id", "tenant_id", "agent_id", "device_id", "tool_name",
	"arguments", "params_hash", "risk_level", "status", "approver_name", "comment",
	"decided_at", "expires_at", "callback_signature", "version", "created_at",
}

func ticketRowVals(status Status, decidedAt any) []any {
	return []any{
		pgUUID, pgDevID, "demo-agent", pgDevID, "set_spindle_speed",
		[]byte(`{"rpm":8000}`), "hash1", 2, status, "", "",
		decidedAt, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "sighash", 1,
		time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func newPGMock(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestPGTicketRepoCreate(t *testing.T) {
	m := newPGMock(t)
	repo := NewPGTicketRepo(m)
	ctx := context.Background()

	in := &ApprovalTicket{
		TenantID: pgDevID, AgentID: "demo-agent", DeviceID: pgDevID,
		ToolName: "set_spindle_speed", Arguments: json.RawMessage(`{"rpm":8000}`),
		RiskLevel: RiskHigh, Status: StatusPending, ParamsHash: "hash1",
		SecretHash: "sighash", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ExpireAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Version: 1,
	}

	m.ExpectQuery(regexp.QuoteMeta(`INSERT INTO adc_approval_tickets
		(tenant_id, agent_id, device_id, tool_name, arguments, params_hash,
		 risk_level, status, approver_name, comment, decided_at, expires_at,
		 callback_signature, version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'PENDING', NULL, NULL, NULL, $8, $9, 1, $10)
		ON CONFLICT (device_id, tool_name, params_hash) WHERE status = 'PENDING' DO NOTHING
		RETURNING `+ticketCols)).
		WithArgs(in.TenantID, in.AgentID, in.DeviceID, in.ToolName, in.Arguments, in.ParamsHash,
			in.RiskLevel, in.ExpireAt, in.SecretHash, in.CreatedAt).
		WillReturnRows(pgxmock.NewRows(ticketRowCols).AddRow(ticketRowVals(StatusPending, nil)...))
	created, err := repo.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.TicketID != pgUUID || created.Status != StatusPending {
		t.Fatalf("unexpected ticket: %+v", created)
	}

	// dedup: insert returns zero rows, re-read returns the existing ticket
	m.ExpectQuery(regexp.QuoteMeta(`INSERT INTO adc_approval_tickets
		(tenant_id, agent_id, device_id, tool_name, arguments, params_hash,
		 risk_level, status, approver_name, comment, decided_at, expires_at,
		 callback_signature, version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'PENDING', NULL, NULL, NULL, $8, $9, 1, $10)
		ON CONFLICT (device_id, tool_name, params_hash) WHERE status = 'PENDING' DO NOTHING
		RETURNING `+ticketCols)).
		WithArgs(in.TenantID, in.AgentID, in.DeviceID, in.ToolName, in.Arguments, in.ParamsHash,
			in.RiskLevel, in.ExpireAt, in.SecretHash, in.CreatedAt).
		WillReturnRows(pgxmock.NewRows(ticketRowCols))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT `+ticketCols+` FROM adc_approval_tickets
		WHERE device_id = $1 AND tool_name = $2 AND params_hash = $3 AND status = 'PENDING'`)).
		WithArgs(in.DeviceID, in.ToolName, in.ParamsHash).
		WillReturnRows(pgxmock.NewRows(ticketRowCols).AddRow(ticketRowVals(StatusPending, nil)...))
	existing, err := repo.Create(ctx, in)
	if !errors.Is(err, ErrTicketDedup) || existing == nil || existing.TicketID != pgUUID {
		t.Fatalf("want ErrTicketDedup with existing ticket, got %v %+v", err, existing)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGTicketRepoTransition(t *testing.T) {
	m := newPGMock(t)
	repo := NewPGTicketRepo(m)
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)

	// approve
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_approval_tickets
			SET status = $2, approver_name = $3, comment = $4, decided_at = $5,
			    version = version + 1, updated_at = $5
			WHERE id = $1 AND status = 'PENDING' AND expires_at > $6 AND version = $7
			RETURNING `+ticketCols)).
		WithArgs(pgUUID, StatusApproved, "boss", "ok", pgxmock.AnyArg(), pgxmock.AnyArg(), 1).
		WillReturnRows(pgxmock.NewRows(ticketRowCols).AddRow(
			pgUUID, pgDevID, "demo-agent", pgDevID, "set_spindle_speed",
			[]byte(`{"rpm":8000}`), "hash1", 2, StatusApproved, "boss", "ok",
			&now, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "sighash", 2,
			time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
	updated, err := repo.Transition(ctx, pgUUID, 1, TransitionCmd{Status: StatusApproved, Approver: "boss", Comment: "ok"})
	if err != nil {
		t.Fatalf("Transition approve: %v", err)
	}
	if updated.Status != StatusApproved || updated.Approver != "boss" {
		t.Fatalf("unexpected ticket: %+v", updated)
	}

	// reject
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_approval_tickets
			SET status = $2, approver_name = $3, comment = $4, decided_at = $5,
			    version = version + 1, updated_at = $5
			WHERE id = $1 AND status = 'PENDING' AND expires_at > $6 AND version = $7
			RETURNING `+ticketCols)).
		WithArgs(pgUUID, StatusRejected, "boss", "", pgxmock.AnyArg(), pgxmock.AnyArg(), 1).
		WillReturnRows(pgxmock.NewRows(ticketRowCols).AddRow(ticketRowVals(StatusRejected, &now)...))
	if _, err := repo.Transition(ctx, pgUUID, 1, TransitionCmd{Status: StatusRejected, Approver: "boss"}); err != nil {
		t.Fatalf("Transition reject: %v", err)
	}

	// expire (scanner path)
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_approval_tickets
			SET status = 'EXPIRED', updated_at = now(), version = version + 1
			WHERE id = $1 AND status = 'PENDING' AND expires_at <= $2 AND version = $3
			RETURNING `+ticketCols)).
		WithArgs(pgUUID, pgxmock.AnyArg(), 1).
		WillReturnRows(pgxmock.NewRows(ticketRowCols).AddRow(ticketRowVals(StatusExpired, nil)...))
	if _, err := repo.Transition(ctx, pgUUID, 1, TransitionCmd{Status: StatusExpired}); err != nil {
		t.Fatalf("Transition expire: %v", err)
	}

	// zero-row update -> re-read -> classify expired (SEC-11)
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_approval_tickets
			SET status = $2, approver_name = $3, comment = $4, decided_at = $5,
			    version = version + 1, updated_at = $5
			WHERE id = $1 AND status = 'PENDING' AND expires_at > $6 AND version = $7
			RETURNING `+ticketCols)).
		WithArgs(pgUUID, StatusApproved, "boss", "", pgxmock.AnyArg(), pgxmock.AnyArg(), 1).
		WillReturnRows(pgxmock.NewRows(ticketRowCols))
	// re-read shows a PENDING ticket whose expiry has passed
	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + ticketCols + ` FROM adc_approval_tickets WHERE id = $1`)).
		WithArgs(pgUUID).
		WillReturnRows(pgxmock.NewRows(ticketRowCols).
			AddRow(pgUUID, pgDevID, "demo-agent", pgDevID, "set_spindle_speed",
				[]byte(`{"rpm":8000}`), "hash1", 2, StatusPending, "", "", nil,
				time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), "sighash", 1, now))
	if _, err := repo.Transition(ctx, pgUUID, 1, TransitionCmd{Status: StatusApproved, Approver: "boss"}); !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("want ErrTicketExpired, got %v", err)
	}

	// zero-row update -> re-read -> already handled
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_approval_tickets
			SET status = $2, approver_name = $3, comment = $4, decided_at = $5,
			    version = version + 1, updated_at = $5
			WHERE id = $1 AND status = 'PENDING' AND expires_at > $6 AND version = $7
			RETURNING `+ticketCols)).
		WithArgs(pgUUID, StatusApproved, "boss", "", pgxmock.AnyArg(), pgxmock.AnyArg(), 1).
		WillReturnRows(pgxmock.NewRows(ticketRowCols))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + ticketCols + ` FROM adc_approval_tickets WHERE id = $1`)).
		WithArgs(pgUUID).
		WillReturnRows(pgxmock.NewRows(ticketRowCols).AddRow(ticketRowVals(StatusApproved, &now)...))
	if _, err := repo.Transition(ctx, pgUUID, 1, TransitionCmd{Status: StatusApproved, Approver: "boss"}); !errors.Is(err, ErrTicketHandled) {
		t.Fatalf("want ErrTicketHandled, got %v", err)
	}

	// invalid target status rejected before any query
	if _, err := repo.Transition(ctx, pgUUID, 1, TransitionCmd{Status: StatusPending}); !errors.Is(err, ErrTicketHandled) {
		t.Fatalf("want ErrTicketHandled, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGTicketRepoGetFindExpired(t *testing.T) {
	m := newPGMock(t)
	repo := NewPGTicketRepo(m)
	ctx := context.Background()

	// Get ok
	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + ticketCols + ` FROM adc_approval_tickets WHERE id = $1`)).
		WithArgs(pgUUID).
		WillReturnRows(pgxmock.NewRows(ticketRowCols).AddRow(ticketRowVals(StatusPending, nil)...))
	if _, err := repo.Get(ctx, pgUUID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Get not found
	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + ticketCols + ` FROM adc_approval_tickets WHERE id = $1`)).
		WithArgs("zzz").
		WillReturnRows(pgxmock.NewRows(ticketRowCols))
	if _, err := repo.Get(ctx, "zzz"); !errors.Is(err, ErrTicketNotFound) {
		t.Fatalf("want ErrTicketNotFound, got %v", err)
	}

	// FindExpired
	m.ExpectQuery(regexp.QuoteMeta(`SELECT `+ticketCols+` FROM adc_approval_tickets
		WHERE status = 'PENDING' AND expires_at <= $1
		ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED`)).
		WithArgs(pgxmock.AnyArg(), 10).
		WillReturnRows(pgxmock.NewRows(ticketRowCols).AddRow(ticketRowVals(StatusPending, nil)...))
	expired, err := repo.FindExpired(ctx, time.Now(), 10)
	if err != nil || len(expired) != 1 {
		t.Fatalf("FindExpired: %v err=%v", expired, err)
	}

	// FindExpired query error
	m.ExpectQuery(regexp.QuoteMeta(`SELECT `+ticketCols+` FROM adc_approval_tickets
		WHERE status = 'PENDING' AND expires_at <= $1
		ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED`)).
		WithArgs(pgxmock.AnyArg(), 10).
		WillReturnError(errors.New("boom"))
	if _, err := repo.FindExpired(ctx, time.Now(), 10); err == nil {
		t.Fatal("want query error")
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

var _ = pgx.ErrNoRows
