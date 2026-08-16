package alerts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const expositionFixture = `# HELP adc_device_online_total Online device count per tenant.
# TYPE adc_device_online_total gauge
adc_device_online_total{tenant="t-a"} 4
adc_device_online_total{tenant="t-b"} 9
# HELP adc_hitl_notify_failures_total Approval card delivery failures by channel (SEC-18).
# TYPE adc_hitl_notify_failures_total counter
adc_hitl_notify_failures_total{channel="wecom"} 12
adc_hitl_notify_failures_total{channel="dingtalk"} 3
# HELP adc_http_requests_total HTTP requests by route pattern and status code.
# TYPE adc_http_requests_total counter
adc_http_requests_total{route="/v1/agent/mcp/tools/call",code="500"} 41
adc_http_requests_total{route="/v1/agent/mcp/tools/call",code="200"} 800
# TYPE adc_pg_pool_connections gauge
adc_pg_pool_connections{state="acquired"} 12
adc_pg_pool_connections{state="max"} 300
# TYPE adc_tool_call_duration_seconds histogram
adc_tool_call_duration_seconds_bucket{le="0.005"} 1
adc_tool_call_duration_seconds_sum 0.1
adc_tool_call_duration_seconds_count 2
# EOF
`

func TestParseExposition(t *testing.T) {
	series, err := ParseExposition([]byte(expositionFixture))
	if err != nil {
		t.Fatalf("ParseExposition: %v", err)
	}
	idx := seriesByName(series)
	if got := idx["adc_device_online_total"]; len(got) != 2 {
		t.Fatalf("device_online series = %d, want 2", len(got))
	}
	if got := idx["adc_pg_pool_connections"]; len(got) != 2 {
		t.Fatalf("pg pool series = %d, want 2", len(got))
	}
	if got := idx["adc_hitl_notify_failures_total"]; len(got) != 2 {
		t.Fatalf("notify series = %d, want 2", len(got))
	}
	fail := idx["adc_hitl_notify_failures_total"]
	if fail[0].Labels["channel"] == "" {
		t.Fatalf("channel label missing: %+v", fail[0])
	}
	if _, ok := idx["adc_tool_call_duration_seconds_bucket"]; !ok {
		t.Fatal("histogram bucket family missing")
	}
}

func TestParseExpositionEdgeLines(t *testing.T) {
	text := []byte("gauge_nan NaN\n# comment\ngauge_inf +Inf\n\nadc_plain_total 7\nbadline\n{only}= 1\n")
	series, err := ParseExposition(text)
	if err != nil {
		t.Fatalf("ParseExposition: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("series = %+v, want only adc_plain_total", series)
	}
	if series[0].Name != "adc_plain_total" || series[0].Value != 7 {
		t.Fatalf("series[0] = %+v", series[0])
	}
}

func TestParseExpositionQuotedLabel(t *testing.T) {
	series, err := ParseExposition([]byte(`adc_x_total{code="500"} 3` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].Labels["code"] != "500" || series[0].Value != 3 {
		t.Fatalf("series = %+v", series)
	}
}

func TestHTTPReader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(expositionFixture))
	}))
	defer srv.Close()

	r := NewHTTPReader(srv.URL + "/metrics")
	series, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(series) == 0 {
		t.Fatal("Read returned no series")
	}

	// Non-2xx surfaces as an error.
	r404 := NewHTTPReader(srv.URL + "/nope")
	if _, err := r404.Read(context.Background()); err == nil {
		t.Fatal("Read on 404 returned nil error")
	}
}

func TestSumMatching(t *testing.T) {
	series, _ := ParseExposition([]byte(expositionFixture))
	family := seriesByName(series)

	// Tenant-labeled family: only the rule tenant counts (fixture labels
	// use "t-a"/"t-b" as tenant values).
	v, matched := sumMatching(family["adc_device_online_total"], "t-a", nil)
	if !matched || v != 4 {
		t.Fatalf("sumMatching(device_online, t-a) = %v, %v; want 4, true", v, matched)
	}
	// Unknown tenant on a tenant-labeled family: no series match.
	_, matched = sumMatching(family["adc_device_online_total"], "00000000-0000-4000-8000-000000000099", nil)
	if matched {
		t.Fatal("sumMatching(device_online, unknown) matched, want no data")
	}
	// Label filters narrow the channel.
	v, matched = sumMatching(family["adc_hitl_notify_failures_total"], "", map[string]string{"channel": "wecom"})
	if !matched || v != 12 {
		t.Fatalf("sumMatching(notify, channel=wecom) = %v, %v; want 12, true", v, matched)
	}
	// Non-tenant family ignores the tenant dimension entirely.
	v, matched = sumMatching(family["adc_pg_pool_connections"], testTenantA, nil)
	if !matched || v != 312 {
		t.Fatalf("sumMatching(pg_pool) = %v, %v; want 312, true", v, matched)
	}
}

func TestPayloadJSON(t *testing.T) {
	ev := Event{
		ID:        "e1",
		RuleID:    "r1",
		RuleName:  "pool",
		TenantID:  testTenantA,
		Metric:    "adc_pg_pool_connections",
		Operator:  OpGreater,
		Threshold: 0.8,
		Observed:  0.9,
		Severity:  SeverityP1,
		FiredAt:   fixedClock,
	}
	raw, err := json.Marshal(payload(&ev))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["severity"] != "P1" || m["alert_name"] != "pool" {
		t.Fatalf("payload = %s", raw)
	}
	if _, ok := m["message"]; !ok {
		t.Fatalf("payload missing message: %s", raw)
	}
}
