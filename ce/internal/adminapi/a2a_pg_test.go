package adminapi

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func TestPGA2ATaskDecisionApproveUsesTenantCASAndRealActor(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	now := time.Now().UTC()
	tenantID, taskID, userID := uuidOf(1), uuidOf(2), uuidOf(3)
	mock.ExpectQuery(regexp.QuoteMeta("WITH actor AS (")).
		WithArgs(tenantID, taskID, int64(4), "working", userID, "approved; awaiting execution result", "reviewed").
		WillReturnRows(pgxmock.NewRows([]string{"task_id", "tenant_id", "state", "version", "approver_user_id", "approver_identity", "decision_reason", "message", "updated_at", "decided_at"}).
			AddRow(taskID, tenantID, "working", int64(5), userID, "Alice", "reviewed", "approved; awaiting execution result", now, now))

	got, err := NewPGA2ATaskDecisionRepo(mock).Decide(context.Background(), tenantID, taskID, 4, "approve", userID, "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "working" || got.Version != 5 || got.ApproverUserID != userID || got.ApproverIdentity != "Alice" {
		t.Fatalf("decision=%+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPGA2ATaskDecisionDistinguishesConflictAndCrossTenantNotFound(t *testing.T) {
	for _, test := range []struct {
		name   string
		exists bool
		want   error
	}{
		{name: "conflict", exists: true, want: ErrA2ATaskConflict},
		{name: "cross tenant", exists: false, want: ErrA2ATaskNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()
			mock.ExpectQuery(regexp.QuoteMeta("WITH actor AS (")).
				WithArgs(uuidOf(1), uuidOf(2), int64(1), "rejected", uuidOf(3), "rejected by approver", "unsafe").
				WillReturnError(pgx.ErrNoRows)
			mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM adc_a2a_tasks")).
				WithArgs(uuidOf(1), uuidOf(2)).
				WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(test.exists))
			_, err = NewPGA2ATaskDecisionRepo(mock).Decide(context.Background(), uuidOf(1), uuidOf(2), 1, "reject", uuidOf(3), "unsafe")
			if err != test.want {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
