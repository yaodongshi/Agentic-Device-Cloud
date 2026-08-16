// Repository seam for the HITL approval ticket aggregate (design/31 LLD
// 3.3.2). PostgreSQL is the source of truth (ADR-05); PGTicketRepo performs
// every decision as a single-statement conditional UPDATE whose predicate
// mirrors design/32 6.1 exactly: WHERE status='PENDING' AND expires_at > now
// AND version = want. The pure helpers CheckTransition, ApplyTransition and
// ClassifyCASFailure keep the CAS semantics testable without a database and
// are shared with the in-memory repository used in tests and degraded
// single-instance mode.

package approval

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Repository outcome errors for the CAS decision path.
var (
	// ErrTicketNotFound is returned when no ticket matches the id.
	ErrTicketNotFound = errors.New("approval: ticket not found")
	// ErrTicketHandled is returned when the ticket was already decided
	// (status no longer PENDING) or the optimistic lock version does not
	// match. This is what makes repeated callbacks harmless (SEC-01,
	// HITL-010).
	ErrTicketHandled = errors.New("approval: ticket already handled")
	// ErrTicketExpired is returned when the decision arrives after ExpireAt
	// (SEC-11, HITL-009).
	ErrTicketExpired = errors.New("approval: ticket expired")
	// ErrTicketDedup is returned by Create together with the existing PENDING
	// ticket when (device_id, tool_name, params_hash) already has an
	// in-flight ticket (design/32 6.3; the LLD error table maps it to HTTP
	// 200 with the existing ticket).
	ErrTicketDedup = errors.New("approval: duplicate pending ticket")
)

// TransitionCmd is the decision payload for TicketRepo.Transition
// (design/31 LLD 3.3.2). Approver is the snapshot of the deciding human
// (design/32 3.13: approver_name is snapshotted at decision time so history
// survives personnel changes).
type TransitionCmd struct {
	Status   Status
	Approver string
	Comment  string
}

// TicketRepo persists the approval aggregate with CAS atomicity
// (design/31 LLD 3.3.2, design/32 6.1). Transition resolves a decision: it
// succeeds only for a PENDING, not-expired ticket whose version matches
// wantVersion, mirroring the guards in CheckTransition.
type TicketRepo interface {
	Create(ctx context.Context, t *ApprovalTicket) (*ApprovalTicket, error)
	Transition(ctx context.Context, ticketID string, wantVersion int, cmd TransitionCmd) (*ApprovalTicket, error)
	Get(ctx context.Context, ticketID string) (*ApprovalTicket, error)
	FindExpired(ctx context.Context, now time.Time, limit int) ([]ApprovalTicket, error)
}

// CheckTransition applies the CAS guards of design/32 6.1 to an in-memory
// ticket snapshot, mirroring the repository's single-statement conditional
// UPDATE predicate: the ticket must still be PENDING, its version must match
// wantVersion, and (unless the target is EXPIRED) it must not be expired. It
// returns the precise failure reason: ErrTicketHandled for a non-PENDING
// status or a version mismatch, ErrTicketExpired when the decision arrived
// after ExpireAt. The EXPIRED target is exempt from the clock check on
// purpose: expiry scanning and device-offline invalidation (HITL-011) both
// land in EXPIRED.
func CheckTransition(t *ApprovalTicket, wantVersion int, now time.Time, to Status) error {
	switch {
	case t.Status != StatusPending:
		return ErrTicketHandled
	case t.Version != wantVersion:
		return ErrTicketHandled
	case to != StatusExpired && !now.Before(t.ExpireAt):
		return ErrTicketExpired
	case !ValidateTransition(t.Status, to):
		return ErrTicketHandled
	}
	return nil
}

// ApplyTransition validates the CAS guards against the snapshot and applies
// the state machine step, mutating t in place. In-memory repositories call
// this under the same lock that emulates the PG single-statement update; the
// PG repository enforces the guards in SQL and only uses ClassifyCASFailure
// to explain a zero-row update. On success it snapshots Approver/Comment,
// bumps Version and, for decided (non-EXPIRED) outcomes, stamps DecidedAt.
func ApplyTransition(t *ApprovalTicket, cmd TransitionCmd, wantVersion int, now time.Time) error {
	if err := CheckTransition(t, wantVersion, now, cmd.Status); err != nil {
		return err
	}
	t.Status = cmd.Status
	t.Approver = cmd.Approver
	t.Comment = cmd.Comment
	t.Version++
	if cmd.Status != StatusExpired {
		decided := now.UTC()
		t.DecidedAt = &decided
	}
	return nil
}

// ClassifyCASFailure explains a conditional UPDATE that matched zero rows
// (design/31 3.3.3): the PG repository re-reads the row and classifies the
// outcome. Precedence: an already-decided status or a version mismatch means
// the ticket was handled (HITL-010) — decided tickets keep reporting handled
// even after they expire; a still-PENDING ticket whose expiry has passed
// reports expired (SEC-11).
func ClassifyCASFailure(t *ApprovalTicket, wantVersion int, now time.Time) error {
	switch {
	case t == nil:
		return ErrTicketNotFound
	case t.Status != StatusPending || t.Version != wantVersion:
		return ErrTicketHandled
	case !now.Before(t.ExpireAt):
		return ErrTicketExpired
	default:
		return ErrTicketHandled
	}
}

// ticketCols is the column projection that rebuilds an ApprovalTicket. UUID
// columns are cast to text because pgx v5.10 has no binary uuid-to-string
// scan plan. callback_signature holds the SHA-256 of the ticket secret (SEC-13:
// only a hash of the key may be persisted, design/33 1.4). Nullable columns
// are coalesced so plain-value scan targets never see NULL (integration
// B-12 P1 fix).
const ticketCols = `id::text, tenant_id::text, COALESCE(agent_id,''), device_id::text, tool_name,
	arguments, params_hash, risk_level, status, COALESCE(approver_name,''), COALESCE(comment,''),
	decided_at, expires_at, callback_signature, version, created_at`

// poolQueryer is the minimal pgx pool surface PGTicketRepo uses.
// *pgxpool.Pool satisfies it in production; unit tests inject pgxmock.
type poolQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PGTicketRepo is the PostgreSQL TicketRepo implementation (ADR-05,
// design/32 6.1).
type PGTicketRepo struct {
	pool poolQueryer
}

// NewPGTicketRepo builds a repository over an existing pgx pool.
func NewPGTicketRepo(pool poolQueryer) *PGTicketRepo {
	return &PGTicketRepo{pool: pool}
}

// scanTicket rebuilds an ApprovalTicket from a ticketCols projection row.
func scanTicket(row pgx.Row) (*ApprovalTicket, error) {
	var t ApprovalTicket
	var decidedAt *time.Time
	err := row.Scan(
		&t.TicketID, &t.TenantID, &t.AgentID, &t.DeviceID, &t.ToolName,
		&t.Arguments, &t.ParamsHash, &t.RiskLevel, &t.Status, &t.Approver,
		&t.Comment, &decidedAt, &t.ExpireAt, &t.SecretHash, &t.Version, &t.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	t.DecidedAt = decidedAt
	return &t, nil
}

// Create inserts a new ticket. When a PENDING ticket with the same
// (device_id, tool_name, params_hash) already exists, the partial unique
// index uq_tickets_pending_dedup (design/32 6.3) blocks the insert and the
// existing ticket is returned with ErrTicketDedup so the caller can reuse it.
func (r *PGTicketRepo) Create(ctx context.Context, t *ApprovalTicket) (*ApprovalTicket, error) {
	row := r.pool.QueryRow(ctx, `INSERT INTO adc_approval_tickets
		(tenant_id, agent_id, device_id, tool_name, arguments, params_hash,
		 risk_level, status, approver_name, comment, decided_at, expires_at,
		 callback_signature, version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'PENDING', NULL, NULL, NULL, $8, $9, 1, $10)
		ON CONFLICT (device_id, tool_name, params_hash) WHERE status = 'PENDING' DO NOTHING
		RETURNING `+ticketCols,
		t.TenantID, t.AgentID, t.DeviceID, t.ToolName, t.Arguments, t.ParamsHash,
		t.RiskLevel, t.ExpireAt, t.SecretHash, t.CreatedAt)
	created, err := scanTicket(row)
	if err == nil {
		return created, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	existing := r.pool.QueryRow(ctx, `SELECT `+ticketCols+` FROM adc_approval_tickets
		WHERE device_id = $1 AND tool_name = $2 AND params_hash = $3 AND status = 'PENDING'`,
		t.DeviceID, t.ToolName, t.ParamsHash)
	ticket, err := scanTicket(existing)
	if err != nil {
		return nil, err
	}
	return ticket, ErrTicketDedup
}

// Transition resolves a decision with one conditional UPDATE (design/32 6.1).
// Zero rows mean the CAS predicate failed; the ticket is re-read and the
// failure classified as already-handled or expired (design/31 3.3.3).
func (r *PGTicketRepo) Transition(ctx context.Context, ticketID string, wantVersion int, cmd TransitionCmd) (*ApprovalTicket, error) {
	now := time.Now().UTC()
	var row pgx.Row
	switch cmd.Status {
	case StatusApproved, StatusRejected:
		row = r.pool.QueryRow(ctx, `UPDATE adc_approval_tickets
			SET status = $2, approver_name = $3, comment = $4, decided_at = $5,
			    version = version + 1, updated_at = $5
			WHERE id = $1 AND status = 'PENDING' AND expires_at > $6 AND version = $7
			RETURNING `+ticketCols,
			ticketID, cmd.Status, cmd.Approver, cmd.Comment, now, now, wantVersion)
	case StatusExpired:
		// Scanner path (design/32 6.1): only expired PENDING tickets, no
		// decided_at stamp.
		row = r.pool.QueryRow(ctx, `UPDATE adc_approval_tickets
			SET status = 'EXPIRED', updated_at = now(), version = version + 1
			WHERE id = $1 AND status = 'PENDING' AND expires_at <= $2 AND version = $3
			RETURNING `+ticketCols,
			ticketID, now, wantVersion)
	default:
		return nil, ErrTicketHandled
	}
	updated, err := scanTicket(row)
	if err == nil {
		return updated, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	cur, err := r.Get(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	return nil, ClassifyCASFailure(cur, wantVersion, now)
}

// Get loads a ticket by id.
func (r *PGTicketRepo) Get(ctx context.Context, ticketID string) (*ApprovalTicket, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+ticketCols+` FROM adc_approval_tickets WHERE id = $1`, ticketID)
	t, err := scanTicket(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTicketNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// FindExpired lists expired PENDING tickets, oldest first, FOR UPDATE SKIP
// LOCKED so concurrent scanner instances never fight over the same rows
// (design/31 3.3.7, design/32 6.1).
func (r *PGTicketRepo) FindExpired(ctx context.Context, now time.Time, limit int) ([]ApprovalTicket, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+ticketCols+` FROM adc_approval_tickets
		WHERE status = 'PENDING' AND expires_at <= $1
		ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApprovalTicket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}
