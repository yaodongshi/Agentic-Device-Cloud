package adminapi

// PostgreSQL repositories for the three optional Admin API seams that
// lacked production implementations (design/80 B-04/B-06): the tool
// risk/enable configuration repo, the approval policy repo and the audit
// query/export repo. Before this file, the integration package supplied
// private copies of the tool and audit repos; those copies are now the
// production implementations living here (the integration env still
// carries its own for assembly, which is harmless duplication).

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// pgToolRepo: PostgreSQL ToolRepo (design/80 B-04, FR-006). Persists
// risk_level / is_enabled with risk_changed_by (SEC-21 real subject) and
// partial-failure outcomes.
// ---------------------------------------------------------------------------

type pgToolRepo struct {
	pool *pgxpool.Pool
}

// NewPGToolRepo builds a PG tool repo over an existing pgx pool.
func NewPGToolRepo(pool *pgxpool.Pool) *pgToolRepo {
	return &pgToolRepo{pool: pool}
}

const toolCols = `id::text, tenant_id::text, device_id::text, tool_name,
	COALESCE(display_name,''), COALESCE(description,''), input_schema,
	risk_level, COALESCE(risk_changed_by::text,''), schema_version, is_enabled, updated_at`

func scanTool(row pgx.Row) (*DeviceTool, error) {
	var t DeviceTool
	var schema []byte
	err := row.Scan(&t.ID, &t.TenantID, &t.DeviceID, &t.ToolName,
		&t.DisplayName, &t.Description, &schema, &t.RiskLevel,
		&t.RiskChangedBy, &t.SchemaVersion, &t.IsEnabled, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if len(schema) > 0 {
		if err := json.Unmarshal(schema, &t.InputSchema); err != nil {
			return nil, err
		}
	}
	return &t, nil
}

// ListTools pages the tool ledger of one device (design/33 3.1.10).
func (r *pgToolRepo) ListTools(ctx context.Context, deviceID string, p Page) ([]DeviceTool, int, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+toolCols+`, count(*) OVER () AS total
		FROM adc_device_tools WHERE device_id = $1::uuid
		ORDER BY tool_name LIMIT $2 OFFSET $3`,
		deviceID, p.Size, (p.Number-1)*p.Size)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var (
		out   []DeviceTool
		total int
	)
	for rows.Next() {
		var t DeviceTool
		var schema []byte
		if err := rows.Scan(&t.ID, &t.TenantID, &t.DeviceID, &t.ToolName,
			&t.DisplayName, &t.Description, &schema, &t.RiskLevel,
			&t.RiskChangedBy, &t.SchemaVersion, &t.IsEnabled, &t.UpdatedAt, &total); err != nil {
			return nil, 0, err
		}
		if len(schema) > 0 {
			_ = json.Unmarshal(schema, &t.InputSchema)
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// GetTool loads one tool by (device, name).
func (r *pgToolRepo) GetTool(ctx context.Context, deviceID, toolName string) (*DeviceTool, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+toolCols+` FROM adc_device_tools
		WHERE device_id = $1::uuid AND tool_name = $2`, deviceID, toolName)
	t, err := scanTool(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrToolNotFound
	}
	return t, err
}

// UpdateTools applies changes one by one; a missing tool becomes a failed
// outcome carrying ErrToolNotFound while the rest still apply
// (design/33 3.1.11 partial-update semantics).
func (r *pgToolRepo) UpdateTools(ctx context.Context, deviceID string, changes []ToolChange, changedBy string) ([]ToolUpdateOutcome, error) {
	out := make([]ToolUpdateOutcome, 0, len(changes))
	for _, c := range changes {
		var (
			risk   any // nil = keep current risk_level
			enab   any // nil = keep current is_enabled
			byUser any // nil = not a risk change
		)
		if c.RiskLevel != nil {
			risk = *c.RiskLevel
			byUser = changedBy
		}
		if c.IsEnabled != nil {
			enab = *c.IsEnabled
		}
		row := r.pool.QueryRow(ctx, `UPDATE adc_device_tools
			SET risk_level = COALESCE($2, risk_level),
			    is_enabled = COALESCE($3, is_enabled),
			    risk_changed_by = COALESCE($4::uuid, risk_changed_by),
			    updated_at = now()
			WHERE device_id = $1::uuid AND tool_name = $5
			RETURNING `+toolCols, deviceID, risk, enab, byUser, c.ToolName)
		updated, err := scanTool(row)
		if errors.Is(err, pgx.ErrNoRows) {
			out = append(out, ToolUpdateOutcome{ToolName: c.ToolName, Err: ErrToolNotFound})
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, ToolUpdateOutcome{ToolName: c.ToolName, Tool: updated})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// pgPolicyRepo: PostgreSQL PolicyRepo (design/80 B-04, FR-007 V1). The
// policy lives in adc_tenants.metadata under the "approval_policy" key
// (see policies.go package comment); writes MERGE jsonb keys
// (metadata || $policy) so the quota fields pgTenantRepo stores in the
// same column survive.
// ---------------------------------------------------------------------------

type pgPolicyRepo struct {
	pool *pgxpool.Pool
}

// NewPGPolicyRepo builds a PG policy repo over an existing pgx pool.
func NewPGPolicyRepo(pool *pgxpool.Pool) *pgPolicyRepo {
	return &pgPolicyRepo{pool: pool}
}

// policyDoc is the JSONB document stored under metadata.approval_policy.
type policyDoc struct {
	ApprovalTimeoutSec int       `json:"approval_timeout_sec"`
	Approvers          []string  `json:"approvers"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// GetPolicy returns the stored policy or the V1 defaults when the key is
// absent; unknown tenants answer ErrTenantNotFound (policies.go
// contract). A stored doc with a zero timeout falls back to the default
// so a hand-edited row cannot violate the sanity window.
func (r *pgPolicyRepo) GetPolicy(ctx context.Context, tenantID string) (*ApprovalPolicy, error) {
	var raw []byte
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(metadata->'approval_policy', 'null'::jsonb)
		  FROM adc_tenants
		 WHERE id = $1::uuid AND deleted_at IS NULL`, tenantID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return defaultPolicy(tenantID), nil
	}
	var doc policyDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("adminapi: decode approval policy: %w", err)
	}
	if doc.ApprovalTimeoutSec <= 0 {
		doc.ApprovalTimeoutSec = defaultApprovalTimeoutSec
	}
	return &ApprovalPolicy{
		TenantID:           tenantID,
		ApprovalTimeoutSec: doc.ApprovalTimeoutSec,
		Approvers:          doc.Approvers,
		UpdatedAt:          doc.UpdatedAt,
	}, nil
}

// SetPolicy merges the policy document into metadata (metadata || doc,
// never a replace) and returns the stored policy with the DB-stamped
// UpdatedAt. Unknown tenants answer ErrTenantNotFound.
func (r *pgPolicyRepo) SetPolicy(ctx context.Context, tenantID string, p *ApprovalPolicy) (*ApprovalPolicy, error) {
	wrapped, err := json.Marshal(map[string]policyDoc{
		"approval_policy": {
			ApprovalTimeoutSec: p.ApprovalTimeoutSec,
			Approvers:          p.Approvers,
			UpdatedAt:          time.Now().UTC(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("adminapi: encode approval policy: %w", err)
	}
	var updatedAt time.Time
	err = r.pool.QueryRow(ctx, `UPDATE adc_tenants
		SET metadata = metadata || $2::jsonb, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING updated_at`, tenantID, string(wrapped)).Scan(&updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ApprovalPolicy{
		TenantID:           tenantID,
		ApprovalTimeoutSec: p.ApprovalTimeoutSec,
		Approvers:          append([]string{}, p.Approvers...),
		UpdatedAt:          updatedAt,
	}, nil
}

// ---------------------------------------------------------------------------
// pgAuditQueryRepo: PostgreSQL AuditQueryRepo (design/80 B-06, FR-013).
// Rides the partition index (idx_audit_tenant_time) and the
// (created_at, id) keyset cursor of design/33 1.6.
// ---------------------------------------------------------------------------

type pgAuditQueryRepo struct {
	pool *pgxpool.Pool
}

// NewPGAuditQueryRepo builds a PG audit query repo over an existing pgx
// pool.
func NewPGAuditQueryRepo(pool *pgxpool.Pool) *pgAuditQueryRepo {
	return &pgAuditQueryRepo{pool: pool}
}

// auditSelect is the shared column projection (mirrors AuditLog field
// order; device_code comes from the ledger join). Nullable text columns
// are COALESCE'd because AuditLog models them as plain strings (pgx
// cannot scan NULL into *string).
const auditSelect = `
SELECT a.id::text, a.tenant_id::text, a.event_type, a.actor_type,
       COALESCE(a.actor_id, ''),
       COALESCE(a.api_key_id::text, ''), COALESCE(a.device_id::text, ''),
       COALESCE(d.device_code, ''),
       COALESCE(a.tool_name, ''), a.risk_level,
       COALESCE(a.request_params, '{}'::jsonb),
       COALESCE(a.response_payload, '{}'::jsonb),
       a.response_truncated, a.execution_duration_ms, a.status,
       COALESCE(a.hitl_ticket_id::text, ''), COALESCE(a.hitl_approver, ''),
       COALESCE(a.hitl_comment, ''), COALESCE(a.exemption_basis, ''),
       a.request_id::text, a.created_at
  FROM adc_audit_logs a
  LEFT JOIN adc_devices d ON d.id = a.device_id`

// auditWhere builds the WHERE clause shared by Query/Count/Export. All
// values travel as parameters (SEC-20: no string concatenation); add
// appends the values first and then renders integer placeholder indices
// so multi-placeholder conditions index correctly.
func auditWhere(f AuditFilter, args *[]any) string {
	var conds []string
	add := func(sql string, vals ...any) {
		start := len(*args) + 1
		*args = append(*args, vals...)
		nums := make([]any, 0, len(vals))
		for i := range vals {
			nums = append(nums, start+i)
		}
		conds = append(conds, fmt.Sprintf(sql, nums...))
	}
	add(`a.tenant_id = $%d::uuid`, f.TenantID)
	if f.TimeFrom != nil {
		add(`a.created_at >= $%d`, *f.TimeFrom)
	}
	if f.TimeTo != nil {
		add(`a.created_at <= $%d`, *f.TimeTo)
	}
	if f.DeviceID != "" {
		add(`a.device_id = $%d::uuid`, f.DeviceID)
	}
	if f.DeviceCode != "" {
		add(`d.device_code = $%d`, f.DeviceCode)
	}
	if f.ToolName != "" {
		add(`a.tool_name = $%d`, f.ToolName)
	}
	if f.Status != "" {
		add(`a.status = $%d`, f.Status)
	}
	if f.EventType != "" {
		add(`a.event_type = $%d`, f.EventType)
	}
	if f.AgentID != "" {
		add(`a.actor_id = $%d`, f.AgentID)
	}
	if f.Keyword != "" {
		pattern := "%" + f.Keyword + "%"
		add(`(a.tool_name ILIKE $%d OR a.actor_id ILIKE $%d OR a.hitl_approver ILIKE $%d OR a.hitl_comment ILIKE $%d)`,
			pattern, pattern, pattern, pattern)
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

func scanAuditLog(rows pgx.Rows) (*AuditLog, error) {
	var l AuditLog
	var params, payload []byte
	err := rows.Scan(
		&l.ID, &l.TenantID, &l.EventType, &l.ActorType, &l.ActorID,
		&l.ApiKeyID, &l.DeviceID, &l.DeviceCode, &l.ToolName, &l.RiskLevel,
		&params, &payload, &l.ResponseTruncated, &l.ExecutionDurationMS,
		&l.Status, &l.HitlTicketID, &l.HitlApprover, &l.HitlComment,
		&l.ExemptReason, &l.RequestID, &l.CreatedAt)
	if err != nil {
		return nil, err
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &l.RequestParams); err != nil {
			return nil, err
		}
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &l.ResponsePayload); err != nil {
			return nil, err
		}
	}
	return &l, nil
}

// Query returns one keyset window ordered created_at DESC, id DESC
// (design/33 1.6). One extra row is fetched to decide NextCursor.
func (r *pgAuditQueryRepo) Query(ctx context.Context, f AuditFilter, after *AuditCursor, limit int) (*AuditPage, error) {
	var args []any
	where := auditWhere(f, &args)
	if after != nil {
		args = append(args, after.CreatedAt, after.ID)
		where += fmt.Sprintf(` AND (a.created_at, a.id) < ($%d, $%d::uuid)`, len(args)-1, len(args))
	}
	args = append(args, limit+1)
	q := auditSelect + where + fmt.Sprintf(` ORDER BY a.created_at DESC, a.id DESC LIMIT $%d`, len(args))
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &AuditPage{}
	for rows.Next() {
		l, err := scanAuditLog(rows)
		if err != nil {
			return nil, err
		}
		if len(page.Items) == limit {
			page.NextCursor = &AuditCursor{CreatedAt: page.Items[len(page.Items)-1].CreatedAt, ID: page.Items[len(page.Items)-1].ID}
			break
		}
		page.Items = append(page.Items, *l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return page, nil
}

// Count reports the rows matching the filter (export cap check).
func (r *pgAuditQueryRepo) Count(ctx context.Context, f AuditFilter) (int, error) {
	var args []any
	where := auditWhere(f, &args)
	var n int
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM adc_audit_logs a
		LEFT JOIN adc_devices d ON d.id = a.device_id`+where, args...).Scan(&n)
	return n, err
}

// Export streams matching rows as CSV capped at maxRows (design/80 B-06
// sync export model); exceeding the cap aborts with the shared sentinel.
func (r *pgAuditQueryRepo) Export(ctx context.Context, f AuditFilter, maxRows int, w io.Writer) (int, error) {
	var args []any
	where := auditWhere(f, &args)
	args = append(args, maxRows+1)
	q := auditSelect + where + fmt.Sprintf(` ORDER BY a.created_at DESC, a.id DESC LIMIT $%d`, len(args))
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	cw := csv.NewWriter(w)
	if err := cw.Write(auditCSVHeader); err != nil {
		return 0, err
	}
	written := 0
	for rows.Next() {
		l, err := scanAuditLog(rows)
		if err != nil {
			return 0, err
		}
		if written >= maxRows {
			return written, ErrAuditExportLimitExceeded
		}
		if err := cw.Write(auditCSVRow(*l)); err != nil {
			return written, err
		}
		written++
	}
	cw.Flush()
	return written, cw.Error()
}
