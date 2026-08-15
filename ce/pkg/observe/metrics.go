// The standard ADC metric set (design/60 6.1 RED baseline, design/80 B-09).
package observe

import (
	"net/http"
)

// Standard metric names. The set is the V1.0 RED baseline and is extended
// with two families required by the seed alert rules
// (deploy/prometheus/alert-rules.yml): adc_hitl_notify_failures_total (P1
// push-failure detection) and adc_pg_pool_connections (P2 pool watermark).
const (
	MetricDeviceOnline        = "adc_device_online_total"
	MetricAgentCalls          = "adc_agent_calls_total"
	MetricHITLIntercepted     = "adc_hitl_intercepted_total"
	MetricHITLApproved        = "adc_hitl_approved_total"
	MetricHITLRejected        = "adc_hitl_rejected_total"
	MetricToolCallDuration    = "adc_tool_call_duration_seconds"
	MetricHTTPRequests        = "adc_http_requests_total"
	MetricHTTPRequestDuration = "adc_http_request_duration_seconds"
	MetricNotifyFailures      = "adc_hitl_notify_failures_total"
	MetricPGPoolConnections   = "adc_pg_pool_connections"
)

// DefaultDurationBuckets mirrors prometheus.DefBuckets (seconds); hand-rolled
// so the ce module keeps zero new dependencies (design/80 B-09).
var DefaultDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Set is the assembled ADC metric bundle. Callers wire the fields into seams
// (connector registry, agentapi router/HITL client, notifiers, HTTP
// middleware); nil seams mean the metric is silently skipped.
type Set struct {
	Registry *Registry

	// DeviceOnline counts online devices per tenant (label: tenant).
	DeviceOnline *GaugeVec
	// AgentCalls counts tool call outcomes per tenant and status
	// (labels: tenant, status; status in success/failed/intercepted).
	AgentCalls *CounterVec
	// HITLIntercepted counts high-risk calls intercepted for approval.
	HITLIntercepted *CounterVec
	// HITLApproved counts intercepted calls approved by a human.
	HITLApproved *CounterVec
	// HITLRejected counts intercepted calls rejected or expired fail-closed.
	HITLRejected *CounterVec
	// ToolCallDuration observes device tool call duration in seconds.
	ToolCallDuration *HistogramVec
	// HTTPRequests counts HTTP requests by route pattern and status code
	// (labels: route, code).
	HTTPRequests *CounterVec
	// HTTPRequestDuration observes HTTP request duration in seconds by route
	// and status code (labels: route, code).
	HTTPRequestDuration *HistogramVec
	// NotifyFailures counts approval card delivery failures per channel
	// (label: channel; P1 alert input).
	NotifyFailures *CounterVec
	// PGPoolConnections reports PostgreSQL pool state (labels: state;
	// state in acquired/max; P2 alert input).
	PGPoolConnections *GaugeVec

	// Handler serves the Prometheus text exposition of the set.
	Handler http.Handler
}

// New builds a fresh registry with every standard metric registered.
func New() *Set {
	return newSet(NewRegistry())
}

// newSet registers all standard families on the given registry (shared by
// tests that need to inspect or augment the registry).
func newSet(r *Registry) *Set {
	s := &Set{
		Registry: r,
		DeviceOnline: r.NewGauge(MetricDeviceOnline,
			"Online device count per tenant.", "tenant"),
		AgentCalls: r.NewCounter(MetricAgentCalls,
			"Agent tool call outcomes by tenant and status.", "tenant", "status"),
		HITLIntercepted: r.NewCounter(MetricHITLIntercepted,
			"High-risk tool calls intercepted for human approval."),
		HITLApproved: r.NewCounter(MetricHITLApproved,
			"Intercepted tool calls approved by a human."),
		HITLRejected: r.NewCounter(MetricHITLRejected,
			"Intercepted tool calls rejected or expired fail-closed."),
		ToolCallDuration: r.NewHistogram(MetricToolCallDuration,
			"Device tool call duration in seconds.", DefaultDurationBuckets),
		HTTPRequests: r.NewCounter(MetricHTTPRequests,
			"HTTP requests by route pattern and status code.", "route", "code"),
		HTTPRequestDuration: r.NewHistogram(MetricHTTPRequestDuration,
			"HTTP request duration in seconds.", DefaultDurationBuckets, "route", "code"),
		NotifyFailures: r.NewCounter(MetricNotifyFailures,
			"Approval card delivery failures by channel (SEC-18).", "channel"),
		PGPoolConnections: r.NewGauge(MetricPGPoolConnections,
			"PostgreSQL connection pool state.", "state"),
	}
	s.Handler = r.Handler()
	return s
}
