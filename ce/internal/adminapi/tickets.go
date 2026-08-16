package adminapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/approval"
	"adc.dev/ce/internal/httpx"
)

// TicketsRepo is the read-only ticket surface for the admin console
// (design/33 3.1.15): list with status filter and pagination. Writes stay
// exclusively on the approval service callback path (SEC-01).
type TicketsRepo interface {
	List(ctx context.Context, tenantID, status string, limit, offset int) ([]approval.ApprovalTicket, int, error)
}

// TicketStatus values accepted by the list filter.
var ticketStatusFilter = map[string]bool{
	"PENDING": true, "APPROVED": true, "REJECTED": true, "EXPIRED": true,
}

// handleTicketsList serves GET /v1/admin/approval-tickets.
func (s *Server) handleTicketsList(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	if s.Tickets == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "tickets repository not wired")
		return
	}
	status := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))
	if status != "" && !ticketStatusFilter[status] {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "invalid status filter")
		return
	}
	limit := 20
	if v := r.URL.Query().Get("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "page_size must be between 1 and 100")
			return
		}
		limit = n
	}
	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "page must be >= 1")
			return
		}
		page = n
	}

	items, total, err := s.Tickets.List(r.Context(), tenantID, status, limit, (page-1)*limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "list tickets failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// pgTicketsRepo is the PostgreSQL TicketsRepo riding the approval ticket
// ledger (read-only; transitions stay on the approval service).
type pgTicketsRepo struct {
	pool pgxPooler
}

// NewPGTicketsRepo builds a tickets repo over an existing pool.
func NewPGTicketsRepo(pool pgxPooler) *pgTicketsRepo {
	return &pgTicketsRepo{pool: pool}
}

// ticketColumns mirrors approval's ticket projection; nullable text columns
// are coalesced (pgx cannot scan NULL into string).
const ticketColumns = `id::text, tenant_id::text, COALESCE(agent_id,''), device_id::text,
	tool_name, arguments, params_hash, risk_level, status, COALESCE(approver_name,''),
	COALESCE(comment,''), decided_at, expires_at, callback_signature, version, created_at`

// List returns a window of tickets plus the filtered total.
func (r *pgTicketsRepo) List(ctx context.Context, tenantID, status string, limit, offset int) ([]approval.ApprovalTicket, int, error) {
	var where []string
	var args []any
	where = append(where, "tenant_id = $1::uuid")
	args = append(args, tenantID)
	if status != "" {
		where = append(where, "status = $2")
		args = append(args, status)
	}
	cond := " WHERE " + strings.Join(where, " AND ")

	var total int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM adc_approval_tickets`+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := r.pool.Query(ctx, `SELECT `+ticketColumns+` FROM adc_approval_tickets`+cond+
		` ORDER BY created_at DESC LIMIT $`+strconv.Itoa(len(args)+1)+` OFFSET $`+strconv.Itoa(len(args)+2),
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]approval.ApprovalTicket, 0, limit)
	for rows.Next() {
		var t approval.ApprovalTicket
		if err := rows.Scan(
			&t.TicketID, &t.TenantID, &t.AgentID, &t.DeviceID, &t.ToolName,
			&t.Arguments, &t.ParamsHash, &t.RiskLevel, &t.Status, &t.Approver,
			&t.Comment, &t.DecidedAt, &t.ExpireAt, &t.SecretHash, &t.Version, &t.CreatedAt,
		); err != nil {
			return nil, 0, err
		}
		items = append(items, t)
	}
	return items, total, rows.Err()
}
