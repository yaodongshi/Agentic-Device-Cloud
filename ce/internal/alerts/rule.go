// Package alerts implements the FR-017 runtime alerting slice (design/82
// B2): tenant-scoped threshold alert rules evaluated in-process against the
// standard observe metric set, webhook notification with Valkey-backed
// deduplication (5 minute aggregation window) and a bounded in-memory event
// history served through the Admin API (/v1/admin/alerts/*).
//
// Evaluation model (tradeoffs): the evaluator polls a MetricReader every 30
// seconds instead of querying Prometheus. V1.0 keeps the five seed rules in
// deploy/prometheus/alert-rules.yml for production clusters; this package
// covers the tenant-configurable thresholds of FR-017 without adding a
// Prometheus query dependency. The reader self-scrapes the local /metrics
// exposition (observe renders it with zero dependencies), so no changes to
// pkg/observe were needed and the evaluator sees the exact same data the
// console dashboard renders. Counters are compared by absolute value (not
// rate): rate() semantics would need delta tracking between samples and are
// left to the Prometheus path.
//
// Storage: rules live in adc_tenants.metadata under the "alert_rules" JSONB
// key, the same pattern as the approval policy (internal/adminapi/
// policies.go). No new migration is required, the JSONB merge (metadata ||
// jsonb_build_object(...)) never clobbers sibling keys, and the rules are
// natural tenant-scoped configuration like quotas and policies. A dedicated
// adc_alert_rules table was rejected for the B2 slice because migration
// 0001 is frozen and the rule volume per tenant is tiny (< 50).
package alerts

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Severity is the alert severity tier (design/60 6.3: P1 page, P2
// on-duty, P3 informational).
type Severity string

const (
	SeverityP1 Severity = "P1"
	SeverityP2 Severity = "P2"
	SeverityP3 Severity = "P3"
)

// validSeverities is the allowed severity set (P1-P3).
var validSeverities = map[Severity]bool{SeverityP1: true, SeverityP2: true, SeverityP3: true}

// IsValidSeverity reports whether s is one of P1/P2/P3.
func IsValidSeverity(s Severity) bool { return validSeverities[s] }

// Operator is a threshold comparison operator. <= is deliberately absent:
// every threshold can be expressed with >= on the complementary value and
// the smaller operator set keeps the console select unambiguous.
type Operator string

const (
	OpGreater        Operator = ">"
	OpLess           Operator = "<"
	OpGreaterOrEqual Operator = ">="
)

// validOperators is the allowed comparison set.
var validOperators = map[Operator]bool{OpGreater: true, OpLess: true, OpGreaterOrEqual: true}

// IsValidOperator reports whether op is one of >, <, >=.
func IsValidOperator(op Operator) bool { return validOperators[op] }

// alertMetrics is the metric whitelist of the B2 slice: every family the
// observe standard set exposes that carries a scalar value (counters and
// gauges). Histograms are excluded because a single threshold on a
// histogram has no well-defined meaning without a quantile.
var alertMetrics = map[string]bool{
	"adc_device_online_total":        true, // gauge, labels: tenant
	"adc_agent_calls_total":          true, // counter, labels: tenant,status
	"adc_hitl_intercepted_total":     true, // counter
	"adc_hitl_approved_total":        true, // counter
	"adc_hitl_rejected_total":        true, // counter
	"adc_http_requests_total":        true, // counter, labels: route,code
	"adc_hitl_notify_failures_total": true, // counter, labels: channel
	"adc_pg_pool_connections":        true, // gauge, labels: state
}

// IsAlertMetric reports whether name is a metric a rule may target.
func IsAlertMetric(name string) bool { return alertMetrics[name] }

// AlertMetricNames returns the sorted whitelist (console select options).
func AlertMetricNames() []string {
	out := make([]string, 0, len(alertMetrics))
	for name := range alertMetrics {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// rule bounds (B2 slice, design/82).
const (
	MaxRulesPerTenant = 50
	MaxRuleNameLen    = 64
	MinDurationSec    = 0
	MaxDurationSec    = 3600
	MaxRuleLabels     = 8
	MaxLabelLen       = 64
)

// Rule is one tenant-scoped threshold alert rule. It is configuration only;
// runtime evaluation state lives in the Evaluator.
type Rule struct {
	ID          string            `json:"id"`
	TenantID    string            `json:"tenant_id"`
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

// ErrTenantNotFound is returned by RuleStore implementations when the
// tenant row does not exist (mirrors adminapi.ErrTenantNotFound; the alerts
// package cannot import adminapi without a cycle).
var ErrTenantNotFound = errors.New("alerts: tenant not found")

var ruleIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// labelKeyRe guards label keys against exposition-format breakage.
var labelKeyRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// NewRuleID returns a random UUIDv4 string (crypto/rand only), assigned
// when the Admin API PUTs a rule list.
func NewRuleID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("rule-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:])
}

// Validate checks one rule before it is stored. A zero ID is accepted
// (the handler assigns one); a present ID must be UUID-shaped so round
// trips through GET stay consistent.
func ValidateRule(r *Rule) error {
	if r == nil {
		return errors.New("alerts: nil rule")
	}
	name := strings.TrimSpace(r.Name)
	if name == "" || len(name) > MaxRuleNameLen {
		return fmt.Errorf("alerts: rule name must be 1-%d chars", MaxRuleNameLen)
	}
	if r.ID != "" && !ruleIDRe.MatchString(r.ID) {
		return errors.New("alerts: rule id must be a uuid")
	}
	if !IsAlertMetric(r.Metric) {
		return fmt.Errorf("alerts: metric %q is not in the alert metric whitelist", r.Metric)
	}
	if !IsValidOperator(r.Operator) {
		return fmt.Errorf("alerts: operator must be one of >, <, >=")
	}
	if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) || r.Threshold < 0 {
		return errors.New("alerts: threshold must be a finite non-negative number")
	}
	if r.DurationSec < MinDurationSec || r.DurationSec > MaxDurationSec {
		return fmt.Errorf("alerts: duration_sec must be between %d and %d", MinDurationSec, MaxDurationSec)
	}
	if !IsValidSeverity(r.Severity) {
		return errors.New("alerts: severity must be P1, P2 or P3")
	}
	if len(r.Labels) > MaxRuleLabels {
		return fmt.Errorf("alerts: at most %d label filters", MaxRuleLabels)
	}
	for k, v := range r.Labels {
		if !labelKeyRe.MatchString(k) || len(k) > MaxLabelLen {
			return fmt.Errorf("alerts: label key %q must match %s and be at most %d chars", k, labelKeyRe, MaxLabelLen)
		}
		if len(v) > MaxLabelLen {
			return fmt.Errorf("alerts: label value %q must be at most %d chars", v, MaxLabelLen)
		}
	}
	return nil
}

// Matches reports whether the observed value satisfies the rule.
func (r *Rule) Matches(value float64) bool {
	switch r.Operator {
	case OpGreater:
		return value > r.Threshold
	case OpLess:
		return value < r.Threshold
	case OpGreaterOrEqual:
		return value >= r.Threshold
	}
	return false
}
