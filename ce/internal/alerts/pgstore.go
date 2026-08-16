package alerts

// PostgreSQL RuleStore: rules live in adc_tenants.metadata under the
// "alert_rules" key (see the package comment for the storage decision).
// The JSONB merge (metadata || jsonb_build_object(...)) preserves sibling
// keys such as the approval policy and tenant quota fields, the same
// discipline internal/adminapi/policies.go mandates for its own metadata
// key. Missing tenants surface as ErrTenantNotFound (pgx.ErrNoRows).

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// alertRulesMetaKey is the adc_tenants.metadata JSONB key.
const alertRulesMetaKey = "alert_rules"

// pgxPooler is the minimal pgx surface the PG store needs (*db.Pool and
// pgxmock.PgxPoolIface both satisfy it).
type pgxPooler interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// metaRule is the stored wire shape of one rule inside the JSONB array.
type metaRule struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Metric      string            `json:"metric"`
	Operator    Operator          `json:"operator"`
	Threshold   float64           `json:"threshold"`
	DurationSec int               `json:"duration_sec"`
	Severity    Severity          `json:"severity"`
	Enabled     bool              `json:"enabled"`
	Labels      map[string]string `json:"labels,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// PGRuleStore persists rules in adc_tenants.metadata JSONB.
type PGRuleStore struct {
	pool pgxPooler
	now  func() time.Time
}

// NewPGRuleStore builds a PG rule store over an existing pgx pool.
func NewPGRuleStore(pool pgxPooler) *PGRuleStore {
	return &PGRuleStore{pool: pool, now: time.Now}
}

// decodeMetaRules parses the stored JSONB array, tolerating a missing key
// (empty slice) and corrupt payloads (logged by the caller, treated as
// empty rather than crashing the evaluator).
func decodeMetaRules(raw []byte) ([]Rule, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var metas []metaRule
	if err := json.Unmarshal(raw, &metas); err != nil {
		return nil, err
	}
	out := make([]Rule, 0, len(metas))
	for _, m := range metas {
		out = append(out, Rule{
			ID:          m.ID,
			Name:        m.Name,
			Metric:      m.Metric,
			Operator:    m.Operator,
			Threshold:   m.Threshold,
			DurationSec: m.DurationSec,
			Severity:    m.Severity,
			Enabled:     m.Enabled,
			Labels:      m.Labels,
			CreatedAt:   m.CreatedAt,
			UpdatedAt:   m.UpdatedAt,
		})
	}
	return out, nil
}

// List loads one tenant's rules. ErrTenantNotFound for unknown tenants.
func (r *PGRuleStore) List(ctx context.Context, tenantID string) ([]Rule, error) {
	var raw []byte
	err := r.pool.QueryRow(ctx,
		`SELECT COALESCE(metadata->'alert_rules', '[]'::jsonb) FROM adc_tenants
		 WHERE id = $1::uuid AND deleted_at IS NULL`, tenantID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeMetaRules(raw)
}

// Replace swaps the tenant rule set. The JSONB merge never clobbers
// sibling metadata keys (approval_policy, quota fields).
func (r *PGRuleStore) Replace(ctx context.Context, tenantID string, rules []Rule) error {
	metas := make([]metaRule, 0, len(rules))
	for _, rule := range rules {
		metas = append(metas, metaRule{
			ID:          rule.ID,
			Name:        rule.Name,
			Metric:      rule.Metric,
			Operator:    rule.Operator,
			Threshold:   rule.Threshold,
			DurationSec: rule.DurationSec,
			Severity:    rule.Severity,
			Enabled:     rule.Enabled,
			Labels:      rule.Labels,
			CreatedAt:   rule.CreatedAt,
			UpdatedAt:   rule.UpdatedAt,
		})
	}
	payload, err := json.Marshal(metas)
	if err != nil {
		return err
	}
	tag, err := r.pool.Exec(ctx,
		`UPDATE adc_tenants
		 SET metadata = metadata || jsonb_build_object('alert_rules', $2::jsonb), updated_at = now()
		 WHERE id = $1::uuid AND deleted_at IS NULL`,
		tenantID, string(payload))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrTenantNotFound
	}
	return nil
}

// All sweeps every live tenant for the evaluator. Corrupt metadata on one
// tenant is skipped (logged by the evaluator) instead of aborting the
// whole sweep: alerting must degrade per-tenant, not globally.
func (r *PGRuleStore) All(ctx context.Context) ([]Rule, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id::text, COALESCE(metadata->'alert_rules', '[]'::jsonb)
		 FROM adc_tenants WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var tenantID string
		var raw []byte
		if err := rows.Scan(&tenantID, &raw); err != nil {
			return nil, err
		}
		rules, err := decodeMetaRules(raw)
		if err != nil {
			continue
		}
		for i := range rules {
			rules[i].TenantID = tenantID
		}
		out = append(out, rules...)
	}
	return out, rows.Err()
}
