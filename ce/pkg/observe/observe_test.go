package observe

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain exposition", ct)
	}
	return rec.Body.String()
}

func findLine(body, prefix string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// TestMetricsOutputContainsAllStandardMetrics covers design/80 B-09
// acceptance: the /metrics endpoint emits every standard metric name with
// the documented label shapes.
func TestMetricsOutputContainsAllStandardMetrics(t *testing.T) {
	s := New()
	s.DeviceOnline.With("t-01").Set(3)
	s.AgentCalls.With("t-01", "success").Inc()
	s.AgentCalls.With("t-01", "failed").Inc()
	s.HITLIntercepted.With().Inc()
	s.HITLApproved.With().Inc()
	s.HITLRejected.With().Inc()
	s.ToolCallDuration.With().Observe(0.3)
	s.HTTPRequests.With("GET /v1/test", "200").Add(10)
	s.HTTPRequestDuration.With("GET /v1/test", "200").Observe(0.012)
	s.NotifyFailures.With("wecom").Inc()
	s.PGPoolConnections.With("acquired").Set(5)
	s.PGPoolConnections.With("max").Set(20)

	body := scrape(t, s.Handler)
	for _, want := range []string{
		`# HELP adc_device_online_total`,
		`# TYPE adc_device_online_total gauge`,
		`adc_device_online_total{tenant="t-01"} 3`,
		`# TYPE adc_agent_calls_total counter`,
		`adc_agent_calls_total{tenant="t-01",status="failed"} 1`,
		`adc_agent_calls_total{tenant="t-01",status="success"} 1`,
		`adc_hitl_intercepted_total 1`,
		`adc_hitl_approved_total 1`,
		`adc_hitl_rejected_total 1`,
		`# TYPE adc_tool_call_duration_seconds histogram`,
		`adc_tool_call_duration_seconds_bucket{le="0.25"} 0`,
		`adc_tool_call_duration_seconds_bucket{le="0.5"} 1`,
		`adc_tool_call_duration_seconds_bucket{le="+Inf"} 1`,
		`adc_tool_call_duration_seconds_sum 0.3`,
		`adc_tool_call_duration_seconds_count 1`,
		`adc_http_requests_total{route="GET /v1/test",code="200"} 10`,
		`adc_http_request_duration_seconds_bucket{route="GET /v1/test",code="200",le="0.025"} 1`,
		`adc_http_request_duration_seconds_sum{route="GET /v1/test",code="200"} 0.012`,
		`adc_http_request_duration_seconds_count{route="GET /v1/test",code="200"} 1`,
		`adc_hitl_notify_failures_total{channel="wecom"} 1`,
		`adc_pg_pool_connections{state="acquired"} 5`,
		`adc_pg_pool_connections{state="max"} 20`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n---\n%s", want, body)
		}
	}
}

// TestHistogramBucketsAreCumulative verifies hand-rolled bucket semantics:
// every bucket counts observations <= its upper bound (le), the +Inf bucket
// equals the total count.
func TestHistogramBucketsAreCumulative(t *testing.T) {
	s := New()
	s.ToolCallDuration.With().Observe(0.004)
	s.ToolCallDuration.With().Observe(0.6)
	s.ToolCallDuration.With().Observe(3)
	body := scrape(t, s.Handler)

	wants := map[string]string{
		`adc_tool_call_duration_seconds_bucket{le="0.005"}`: "1",
		`adc_tool_call_duration_seconds_bucket{le="0.01"}`:  "1",
		`adc_tool_call_duration_seconds_bucket{le="0.5"}`:   "1",
		`adc_tool_call_duration_seconds_bucket{le="1"}`:     "2",
		`adc_tool_call_duration_seconds_bucket{le="5"}`:     "3",
		`adc_tool_call_duration_seconds_bucket{le="+Inf"}`:  "3",
		`adc_tool_call_duration_seconds_count`:              "3",
	}
	for prefix, want := range wants {
		line := findLine(body, prefix)
		if !strings.HasSuffix(line, " "+want) {
			t.Errorf("%s = %q, want value %s", prefix, line, want)
		}
	}
}

// TestLabelValueEscaping verifies Prometheus quoted-string escaping of
// backslashes and quotes in label values.
func TestLabelValueEscaping(t *testing.T) {
	r := NewRegistry()
	r.NewGauge("adc_test_escaped", "escaping help", "tenant").With(`a"b\c`).Set(1)
	body := scrape(t, r.Handler())
	if !strings.Contains(body, `adc_test_escaped{tenant="a\"b\\c"} 1`) {
		t.Errorf("escaped label output missing\n---\n%s", body)
	}
}

// TestOutputIsDeterministic ensures repeated scrapes render identical text
// (map iteration order must not leak).
func TestOutputIsDeterministic(t *testing.T) {
	s := New()
	s.DeviceOnline.With("t-01").Set(1)
	s.DeviceOnline.With("t-02").Set(2)
	s.DeviceOnline.With("t-03").Set(3)
	a := scrape(t, s.Handler)
	for i := 0; i < 20; i++ {
		if b := scrape(t, s.Handler); a != b {
			t.Fatalf("scrape output changed between calls\n---\n%s\n---\n%s", a, b)
		}
	}
}

// TestNilMetricsSeamsNoPanic covers the "tests pass nil metrics" contract:
// nil vecs and a nil middleware metric pair must be harmless no-ops.
func TestNilMetricsSeamsNoPanic(t *testing.T) {
	var gv *GaugeVec
	gv.With("x").Set(1)
	gv.Get("x")
	var cv *CounterVec
	cv.With("x").Inc()
	cv.Get("x")
	var hv *HistogramVec
	hv.With("x").Observe(1)
}

// TestDuplicateRegistrationPanics ensures a programming error surfaces
// immediately instead of silently shadowing a metric family.
func TestDuplicateRegistrationPanics(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("adc_dup", "first")
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration did not panic")
		}
	}()
	r.NewCounter("adc_dup", "second")
}

// TestGaugeAddAndSet verifies atomic gauge arithmetic.
func TestGaugeAddAndSet(t *testing.T) {
	s := New()
	g := s.DeviceOnline.With("t")
	g.Set(5)
	g.Add(2)
	g.Add(-1)
	if got := s.DeviceOnline.Get("t"); got != 6 {
		t.Fatalf("gauge = %v, want 6", got)
	}
}
