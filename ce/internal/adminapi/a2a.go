package adminapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/httpx"
)

var (
	ErrA2ATaskNotFound = errors.New("adminapi: A2A task not found")
	ErrA2ATaskConflict = errors.New("adminapi: A2A task version conflict")
)

type A2ATaskDecision struct {
	TaskID           string    `json:"task_id"`
	TenantID         string    `json:"tenant_id"`
	State            string    `json:"state"`
	Version          int64     `json:"version"`
	ApproverUserID   string    `json:"approver_user_id"`
	ApproverIdentity string    `json:"approver_identity"`
	DecisionReason   string    `json:"decision_reason"`
	Message          string    `json:"message"`
	UpdatedAt        time.Time `json:"updated_at"`
	DecidedAt        time.Time `json:"decided_at"`
}

type A2ATaskDecisionRepo interface {
	Decide(context.Context, string, string, int64, string, string, string) (*A2ATaskDecision, error)
}

type pgA2ATaskDecisionRepo struct{ pool pgxPooler }

func NewPGA2ATaskDecisionRepo(pool pgxPooler) *pgA2ATaskDecisionRepo {
	return &pgA2ATaskDecisionRepo{pool: pool}
}

func (r *pgA2ATaskDecisionRepo) Decide(ctx context.Context, tenantID, taskID string, expectedVersion int64, decision, userID, reason string) (*A2ATaskDecision, error) {
	state, message := "working", "approved; awaiting execution result"
	if decision == "reject" {
		state, message = "rejected", "rejected by approver"
	}
	var out A2ATaskDecision
	err := r.pool.QueryRow(ctx, `WITH actor AS (
		SELECT COALESCE(NULLIF(display_name,''),username) AS identity FROM adc_users
		WHERE id=$5::uuid AND status='ACTIVE' AND deleted_at IS NULL
	)
	UPDATE adc_a2a_tasks SET state=$4,message=$6,version=version+1,
		approver_user_id=$5::uuid,approver_identity=(SELECT identity FROM actor),
		decision_reason=$7,updated_at=now(),decided_at=now()
	WHERE tenant_id=$1::uuid AND task_id=$2::uuid AND state='input-required'
		AND version=$3 AND EXISTS(SELECT 1 FROM actor)
	RETURNING task_id::text,tenant_id::text,state,version,approver_user_id::text,
		approver_identity,decision_reason,message,updated_at,decided_at`,
		tenantID, taskID, expectedVersion, state, userID, message, reason).Scan(
		&out.TaskID, &out.TenantID, &out.State, &out.Version, &out.ApproverUserID,
		&out.ApproverIdentity, &out.DecisionReason, &out.Message, &out.UpdatedAt, &out.DecidedAt)
	if err == nil {
		return &out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var exists bool
	if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM adc_a2a_tasks
		WHERE tenant_id=$1::uuid AND task_id=$2::uuid)`, tenantID, taskID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrA2ATaskNotFound
	}
	return nil, ErrA2ATaskConflict
}

type a2aDecisionRequest struct {
	Decision        string `json:"decision"`
	Reason          string `json:"reason"`
	ExpectedVersion int64  `json:"expected_version"`
}

func hasA2ADecisionRole(p *adminauth.Principal) bool {
	for _, role := range p.Roles {
		switch strings.ToLower(role) {
		case "platform_admin", "tenant_admin", "admin", "approver":
			return true
		}
	}
	return false
}

func (s *Server) handleA2ATaskDecision(w http.ResponseWriter, r *http.Request) {
	p, _ := adminauth.FromContext(r.Context())
	if !hasA2ADecisionRole(p) {
		writeError(w, r, http.StatusForbidden, codeForbidden, "forbidden")
		return
	}
	if s.A2ATasks == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "A2A tasks unavailable")
		return
	}
	taskID := r.PathValue("taskID")
	if !requireUUID(w, r, "task_id", taskID) {
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	var req a2aDecisionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Decision = strings.ToLower(strings.TrimSpace(req.Decision))
	req.Reason = strings.TrimSpace(req.Reason)
	if (req.Decision != "approve" && req.Decision != "reject") || req.ExpectedVersion < 1 || len(req.Reason) > 2000 {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "invalid decision, reason, or expected_version")
		return
	}
	result, err := s.A2ATasks.Decide(r.Context(), tenantID, taskID, req.ExpectedVersion, req.Decision, p.UserID, req.Reason)
	switch {
	case errors.Is(err, ErrA2ATaskNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "task not found")
	case errors.Is(err, ErrA2ATaskConflict):
		writeError(w, r, http.StatusConflict, codeConflict, "task state or version conflict")
	case err != nil:
		writeError(w, r, http.StatusInternalServerError, codeInternal, "decide A2A task failed")
	default:
		httpx.WriteJSON(w, http.StatusOK, result)
	}
}
