package approval

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeNotifier fails its first failN calls, then succeeds; it records the
// attempt count and the last payload it saw.
type fakeNotifier struct {
	calls int
	failN int
	err   error
}

func (f *fakeNotifier) SendApprovalCall() error {
	f.calls++
	if f.calls <= f.failN {
		if f.err != nil {
			return f.err
		}
		return errors.New("fake notifier failure")
	}
	return nil
}

func TestWeComCardPayload(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	ticket := newTestTicket(t)
	ticket.AgentID = "prod-scheduling-agent"
	approveURL := "https://adc.example.com/v1/hitl/action?ticket_id=x&decision=approve&sig=s1"
	rejectURL := "https://adc.example.com/v1/hitl/action?ticket_id=x&decision=reject&sig=s2"

	n := NewWeComNotifier(srv.URL)
	if err := n.SendApprovalCard(context.Background(), ticket, approveURL, rejectURL); err != nil {
		t.Fatalf("SendApprovalCard: %v", err)
	}
	if got == nil {
		t.Fatal("server did not receive a payload")
	}
	if got["msgtype"] != "template_card" {
		t.Errorf("msgtype = %v, want template_card", got["msgtype"])
	}
	card, ok := got["template_card"].(map[string]any)
	if !ok {
		t.Fatalf("template_card missing: %#v", got)
	}
	if card["card_type"] != "button_interaction" {
		t.Errorf("card_type = %v, want button_interaction", card["card_type"])
	}
	if card["task_id"] != ticket.TicketID {
		t.Errorf("task_id = %v, want %s", card["task_id"], ticket.TicketID)
	}
	mainTitle, ok := card["main_title"].(map[string]any)
	if !ok {
		t.Fatalf("main_title missing: %#v", card)
	}
	title := mainTitle["title"].(string)
	if title != "ADC 高危设备操作审批" {
		t.Errorf("title = %q", title)
	}
	if strings.Contains(title, "🚨") || strings.Contains(title, "✅") {
		t.Error("card title contains an emoji (GAP-16)")
	}
	rows, ok := card["horizontal_content_list"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("horizontal_content_list missing: %#v", card)
	}
	wantKV := map[string]string{
		"目标设备": ticket.DeviceID,
		"触发工具": ticket.ToolName,
		"调用参数": `{"force":true}`,
		"风险等级": "2（高危，须人工审批）",
		"申请来源": "prod-scheduling-agent",
	}
	found := map[string]bool{}
	for _, r := range rows {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("bad row type: %#v", r)
		}
		key := row["keyname"].(string)
		val := row["value"].(string)
		if want, ok := wantKV[key]; ok {
			if val != want {
				t.Errorf("row %q value = %q, want %q", key, val, want)
			}
			found[key] = true
		}
	}
	for k := range wantKV {
		if !found[k] {
			t.Errorf("row %q missing from card", k)
		}
	}
	buttons, ok := card["button_list"].([]any)
	if !ok || len(buttons) != 2 {
		t.Fatalf("button_list missing or wrong size: %#v", card["button_list"])
	}
	b0 := buttons[0].(map[string]any)
	b1 := buttons[1].(map[string]any)
	if b0["text"] != "核实并执行" || b0["url"] != approveURL {
		t.Errorf("approve button = %#v, want text 核实并执行 url %s", b0, approveURL)
	}
	if b1["text"] != "拦截终止" || b1["url"] != rejectURL {
		t.Errorf("reject button = %#v, want text 拦截终止 url %s", b1, rejectURL)
	}
	if b0["text"] == "✅ 核实并执行" || b1["text"] == "❌ 拦截终止" {
		t.Error("button text contains an emoji (GAP-16)")
	}
}

func TestDingTalkCardPayload(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer srv.Close()

	ticket := newTestTicket(t)
	ticket.AgentID = "scheduler-02"
	approveURL := "https://adc.example.com/v1/hitl/action?ticket_id=x&decision=approve&sig=s1"
	rejectURL := "https://adc.example.com/v1/hitl/action?ticket_id=x&decision=reject&sig=s2"

	n := NewDingTalkNotifier(srv.URL)
	if err := n.SendApprovalCard(context.Background(), ticket, approveURL, rejectURL); err != nil {
		t.Fatalf("SendApprovalCard: %v", err)
	}
	if got["msgtype"] != "actionCard" {
		t.Errorf("msgtype = %v, want actionCard", got["msgtype"])
	}
	card, ok := got["actionCard"].(map[string]any)
	if !ok {
		t.Fatalf("actionCard missing: %#v", got)
	}
	if card["title"] != "ADC 高危设备操作审批" {
		t.Errorf("title = %v", card["title"])
	}
	if card["btnOrientation"] != "0" {
		t.Errorf("btnOrientation = %v, want 0", card["btnOrientation"])
	}
	text := card["text"].(string)
	for _, want := range []string{
		"ADC 设备操作审批请求",
		"**目标设备**: `device-1`",
		"**触发工具**: `power_off`",
		"**风险等级**: `2（高危，须人工审批）`",
		"**申请来源**: `scheduler-02`",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
	btns, ok := card["btns"].([]any)
	if !ok || len(btns) != 2 {
		t.Fatalf("btns missing: %#v", card["btns"])
	}
	if btns[0].(map[string]any)["actionURL"] != approveURL {
		t.Errorf("approve actionURL = %v, want %s", btns[0], approveURL)
	}
	if btns[1].(map[string]any)["actionURL"] != rejectURL {
		t.Errorf("reject actionURL = %v, want %s", btns[1], rejectURL)
	}
}

func TestSendJSONChecksHTTPStatus(t *testing.T) {
	ticket := newTestTicket(t)
	cases := []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{"2xx with ok ack", 200, `{"errcode":0}`, false},
		{"2xx with empty body", 200, ``, false},
		{"2xx with non-ack json", 200, `{"foo":1}`, false},
		{"2xx with business error", 200, `{"errcode":93000}`, true},
		{"5xx", 500, `oops`, true},
		{"4xx", 404, `not found`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			n := NewWeComNotifier(srv.URL)
			err := n.SendApprovalCard(context.Background(), ticket, "u1", "u2")
			if c.wantError && err == nil {
				t.Error("want error, got nil")
			}
			if !c.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestRetryNotifierBackoffAndSuccess(t *testing.T) {
	inner := &fakeNotifier{failN: 2}
	var sleeps []time.Duration
	retry := NewRetryNotifier(&adapterNotifier{inner})
	retry.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	ticket := newTestTicket(t)
	if err := retry.SendApprovalCard(context.Background(), ticket, "u1", "u2"); err != nil {
		t.Fatalf("SendApprovalCard: %v", err)
	}
	if inner.calls != 3 {
		t.Errorf("attempts = %d, want 3", inner.calls)
	}
	if len(sleeps) != 2 || sleeps[0] != time.Second || sleeps[1] != 2*time.Second {
		t.Errorf("backoffs = %v, want [1s 2s]", sleeps)
	}
}

func TestRetryNotifierExhausted(t *testing.T) {
	inner := &fakeNotifier{failN: 99}
	retry := NewRetryNotifier(&adapterNotifier{inner})
	retry.Sleep = func(_ context.Context, d time.Duration) error { return nil }
	ticket := newTestTicket(t)
	err := retry.SendApprovalCard(context.Background(), ticket, "u1", "u2")
	if err == nil {
		t.Fatal("want error after exhausting attempts")
	}
	if !strings.Contains(err.Error(), "3 attempts") {
		t.Errorf("error = %v, want attempts count", err)
	}
	if inner.calls != 3 {
		t.Errorf("attempts = %d, want 3", inner.calls)
	}
}

// adapterNotifier adapts the fake call counter to the Notifier interface.
type adapterNotifier struct{ inner *fakeNotifier }

func (a *adapterNotifier) SendApprovalCard(_ context.Context, _ *ApprovalTicket, _, _ string) error {
	return a.inner.SendApprovalCall()
}

func TestMultiNotifierFanout(t *testing.T) {
	a := &fakeNotifier{failN: 0}
	b := &fakeNotifier{failN: 1}
	m := NewMultiNotifier(&adapterNotifier{a}, nil, &adapterNotifier{b})
	ticket := newTestTicket(t)
	err := m.SendApprovalCard(context.Background(), ticket, "u1", "u2")
	if err == nil {
		t.Fatal("want joined error when one channel fails")
	}
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("calls = %d/%d, want 1/1 (every channel must be attempted)", a.calls, b.calls)
	}
}

func TestDeriveTicketSecretDeterministicAndDistinct(t *testing.T) {
	key := "callback-key-0123456789abcdef"
	id := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	s1 := DeriveTicketSecret(key, id)
	s2 := DeriveTicketSecret(key, id)
	if s1 != s2 {
		t.Error("derivation is not deterministic")
	}
	if len(s1) != 64 {
		t.Errorf("secret length = %d, want 64", len(s1))
	}
	if DeriveTicketSecret(key, "3fa85f64-5717-4562-b3fc-2c963f66afa7") == s1 {
		t.Error("different ticket id produced the same secret")
	}
	if DeriveTicketSecret("another-key-0123456789abcdef", id) == s1 {
		t.Error("different platform key produced the same secret")
	}
	if s1 == id {
		t.Error("derived secret equals the ticket id")
	}
}

func TestBuildActionURLRoundTrip(t *testing.T) {
	secret := DeriveTicketSecret("cbk", "ticket-1")
	expire := fixedClock().Add(DefaultExpiry)
	u := BuildActionURL("https://adc.example.com/", "ticket-1", DecisionApprove, expire, secret)
	if !strings.HasPrefix(u, "https://adc.example.com/v1/hitl/action?") {
		t.Fatalf("url = %s", u)
	}
	for _, want := range []string{
		"ticket_id=ticket-1",
		"decision=approve",
		"expire=" + strconv.FormatInt(expire.Unix(), 10),
		"sig=" + SignCallback(secret, "ticket-1", DecisionApprove, expire),
	} {
		if !strings.Contains(u, want) {
			t.Errorf("url %s missing %s", u, want)
		}
	}
}
