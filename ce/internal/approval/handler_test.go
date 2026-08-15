package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testCallbackKey is the platform callback key shared by the handler and the
// signatures built in these tests.
const testCallbackKey = "test-callback-key-0123456789abcdef"

// newTestMux assembles a handler over the mem repo with a fake clock and
// registers its routes on a fresh mux.
func newTestMux(repo *memTicketRepo, clock *fakeClock, bus *InMemoryEventBus) *http.ServeMux {
	h := NewCallbackHandler(repo, bus, testCallbackKey)
	h.Now = clock.Now
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

// signedBody builds the callback JSON body for a ticket with a signature
// over the derived secret.
func signedBody(id, decision string, expire time.Time, extra map[string]any) map[string]any {
	secret := DeriveTicketSecret(testCallbackKey, id)
	body := map[string]any{
		"ticket_id": id,
		"decision":  decision,
		"signature": SignCallback(secret, id, decision, expire),
		"expire":    expire.Unix(),
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func postCallback(t *testing.T, mux *http.ServeMux, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, CallbackPath, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// subscribeBus registers one subscriber collecting events into a channel.
func subscribeBus(t *testing.T, bus *InMemoryEventBus) (<-chan wakeEvent, context.CancelFunc) {
	t.Helper()
	events := make(chan wakeEvent, 16)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = bus.SubscribeResolved(ctx, func(id string, status Status) {
			events <- wakeEvent{TicketID: id, Status: status}
		})
	}()
	waitForSubscribers(t, bus, 1)
	return events, cancel
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return body.Code
}

func TestCallbackApproveSuccessAndWake(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	events, cancel := subscribeBus(t, bus)
	defer cancel()
	id := seedTicket(t, repo, clock)
	mux := newTestMux(repo, clock, bus)

	w := postCallback(t, mux, signedBody(id, DecisionApprove, clock.Now().Add(DefaultExpiry), map[string]any{
		"approver": "emp_zhangwei",
		"comment":  "已核实工艺单，放行",
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var res struct {
		TicketID string `json:"ticket_id"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.TicketID != id || res.Status != "approved" {
		t.Errorf("response = %+v, want %s/approved", res, id)
	}
	stored, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != StatusApproved || stored.Version != 2 || stored.Approver != "emp_zhangwei" || stored.Comment != "已核实工艺单，放行" {
		t.Errorf("stored ticket = %+v", stored)
	}
	select {
	case ev := <-events:
		if ev.TicketID != id || ev.Status != StatusApproved {
			t.Errorf("wake event = %+v, want %s/APPROVED", ev, id)
		}
	case <-time.After(time.Second):
		t.Fatal("no wake event published after approval (SEC-10)")
	}
}

func TestCallbackReject(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	id := seedTicket(t, repo, clock)
	mux := newTestMux(repo, clock, bus)

	w := postCallback(t, mux, signedBody(id, DecisionReject, clock.Now().Add(DefaultExpiry), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"rejected"`) {
		t.Errorf("body = %s", w.Body.String())
	}
	stored, _ := repo.Get(context.Background(), id)
	if stored.Status != StatusRejected {
		t.Errorf("status = %q, want REJECTED", stored.Status)
	}
}

func TestCallbackBadSignature(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	id := seedTicket(t, repo, clock)
	mux := newTestMux(repo, clock, bus)

	body := signedBody(id, DecisionApprove, clock.Now().Add(DefaultExpiry), nil)
	body["signature"] = strings.Repeat("0", 64)
	w := postCallback(t, mux, body)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if code := errorCode(t, w); code != "12004" {
		t.Errorf("code = %s, want 12004", code)
	}
	// The failed callback must not touch the ticket.
	stored, _ := repo.Get(context.Background(), id)
	if stored.Status != StatusPending {
		t.Errorf("status = %q after failed signature, want PENDING", stored.Status)
	}
}

func TestCallbackExpired(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	// Seed a ticket whose signed expiry is already in the past: the
	// signature verifies but the time window rejects it (SEC-11).
	ticket := newTestTicket(t)
	ticket.CreatedAt = clock.Now()
	ticket.ExpireAt = clock.Now().Add(-time.Minute)
	created, err := repo.Create(context.Background(), ticket)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := created.TicketID
	mux := newTestMux(repo, clock, bus)

	w := postCallback(t, mux, signedBody(id, DecisionApprove, ticket.ExpireAt, nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body=%s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "12003" {
		t.Errorf("code = %s, want 12003", code)
	}
	stored, _ := repo.Get(context.Background(), id)
	if stored.Status != StatusPending {
		t.Errorf("status = %q, want PENDING (the scanner owns expiry)", stored.Status)
	}
}

func TestCallbackDoubleDecisionIdempotent(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	events, cancel := subscribeBus(t, bus)
	defer cancel()
	id := seedTicket(t, repo, clock)
	mux := newTestMux(repo, clock, bus)

	body := signedBody(id, DecisionApprove, clock.Now().Add(DefaultExpiry), nil)
	if w := postCallback(t, mux, body); w.Code != http.StatusOK {
		t.Fatalf("first callback = %d", w.Code)
	}
	// The identical replay is refused with 12002 and does not re-fire the
	// decision (SEC-01 one-shot consumption).
	w := postCallback(t, mux, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("replay status = %d, want 409, body=%s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "12002" {
		t.Errorf("code = %s, want 12002", code)
	}
	stored, _ := repo.Get(context.Background(), id)
	if stored.Status != StatusApproved || stored.Version != 2 {
		t.Errorf("stored = %q/v%d after replay", stored.Status, stored.Version)
	}
	select {
	case ev := <-events:
		if ev.TicketID != id {
			t.Errorf("first wake event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no wake event published after approval (SEC-10)")
	}
	select {
	case <-events:
		t.Error("replayed callback published a second wake event")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCallbackInvalidDecision(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	id := seedTicket(t, repo, clock)
	mux := newTestMux(repo, clock, bus)

	w := postCallback(t, mux, map[string]any{
		"ticket_id": id,
		"decision":  "sneaky",
		"signature": "00",
		"expire":    clock.Now().Add(DefaultExpiry).Unix(),
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if code := errorCode(t, w); code != "10001" {
		t.Errorf("code = %s, want 10001", code)
	}
}

func TestCallbackUnknownTicket(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	mux := newTestMux(repo, clock, bus)

	id := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	w := postCallback(t, mux, signedBody(id, DecisionApprove, clock.Now().Add(DefaultExpiry), nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if code := errorCode(t, w); code != "12001" {
		t.Errorf("code = %s, want 12001", code)
	}
}

func TestCallbackMalformedJSONAndMissingFields(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	mux := newTestMux(repo, clock, bus)

	req := httptest.NewRequest(http.MethodPost, CallbackPath, strings.NewReader("{not json"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed json status = %d, want 400", w.Code)
	}

	for name, body := range map[string]map[string]any{
		"missing signature": {"ticket_id": "x", "decision": "approve", "expire": 1},
		"missing expire":    {"ticket_id": "x", "decision": "approve", "signature": "s"},
		"missing ticket":    {"decision": "approve", "signature": "s", "expire": 1},
	} {
		if w := postCallback(t, mux, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", name, w.Code)
		}
	}
}

// --- landing page (design/20 4.14, F-11) ---

func actionURL(id, decision string, expire time.Time) string {
	secret := DeriveTicketSecret(testCallbackKey, id)
	q := url.Values{}
	q.Set("ticket_id", id)
	q.Set("decision", decision)
	q.Set("expire", strconv.FormatInt(expire.Unix(), 10))
	q.Set("sig", SignCallback(secret, id, decision, expire))
	return ActionPath + "?" + q.Encode()
}

func getPage(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestActionPageValidRendersConfirmForm(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	id := seedTicket(t, repo, clock)
	mux := newTestMux(repo, clock, bus)

	w := getPage(mux, actionURL(id, DecisionApprove, clock.Now().Add(DefaultExpiry)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"审批确认", "确认执行", "name=\"ticket_id\"", "name=\"signature\"", "name=\"expire\"", id, "power_off"} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm page missing %q", want)
		}
	}
	if !strings.Contains(body, `action="/v1/hitl/action"`) {
		t.Error("confirm form does not post back to the action endpoint")
	}
	// A GET must never decide the ticket (SEC-01).
	stored, _ := repo.Get(context.Background(), id)
	if stored.Status != StatusPending {
		t.Errorf("status = %q after GET, want PENDING", stored.Status)
	}
}

func TestActionPageRejectShowsCommentField(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	id := seedTicket(t, repo, clock)
	mux := newTestMux(repo, clock, bus)

	w := getPage(mux, actionURL(id, DecisionReject, clock.Now().Add(DefaultExpiry)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "确认拒绝") || !strings.Contains(body, "comment") {
		t.Errorf("reject page missing confirm button or comment field:\n%s", body)
	}
}

func TestActionPageBadSignature(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	id := seedTicket(t, repo, clock)
	mux := newTestMux(repo, clock, bus)

	u := actionURL(id, DecisionApprove, clock.Now().Add(DefaultExpiry)) + "0" // corrupt sig
	w := getPage(mux, u)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "签名校验失败") {
		t.Errorf("page missing signature failure state:\n%s", body)
	}
	// design/20 4.14: a signature failure must not reveal ticket data.
	if strings.Contains(body, id) || strings.Contains(body, "power_off") {
		t.Error("signature failure page leaks ticket data")
	}
}

func TestActionPageExpiredState(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	ticket := newTestTicket(t)
	ticket.CreatedAt = clock.Now()
	ticket.ExpireAt = clock.Now().Add(-time.Minute)
	created, err := repo.Create(context.Background(), ticket)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mux := newTestMux(repo, clock, bus)

	w := getPage(mux, actionURL(created.TicketID, DecisionApprove, ticket.ExpireAt))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "工单已超时失效") || !strings.Contains(body, "已自动拒绝，指令未执行") {
		t.Errorf("expired page wrong:\n%s", body)
	}
}

func TestActionPageHandledState(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	id := seedTicket(t, repo, clock)
	if _, err := repo.Transition(context.Background(), id, 1, TransitionCmd{Status: StatusRejected, Approver: "zhangwei"}); err != nil {
		t.Fatalf("pre-decide: %v", err)
	}
	mux := newTestMux(repo, clock, bus)

	w := getPage(mux, actionURL(id, DecisionApprove, clock.Now().Add(DefaultExpiry)))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"工单已被处理", "zhangwei", "已拒绝"} {
		if !strings.Contains(body, want) {
			t.Errorf("handled page missing %q:\n%s", want, body)
		}
	}
}

func TestActionConfirmPostDecidesAndWakes(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	events, cancel := subscribeBus(t, bus)
	defer cancel()
	id := seedTicket(t, repo, clock)
	mux := newTestMux(repo, clock, bus)

	expire := clock.Now().Add(DefaultExpiry)
	secret := DeriveTicketSecret(testCallbackKey, id)
	form := url.Values{}
	form.Set("ticket_id", id)
	form.Set("decision", DecisionApprove)
	form.Set("expire", strconv.FormatInt(expire.Unix(), 10))
	form.Set("sig", SignCallback(secret, id, DecisionApprove, expire))
	form.Set("comment", "已核实")
	req := httptest.NewRequest(http.MethodPost, ActionPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"已确认执行", "指令已下发", id} {
		if !strings.Contains(body, want) {
			t.Errorf("success page missing %q:\n%s", want, body)
		}
	}
	stored, err := repo.Get(context.Background(), id)
	if err != nil || stored.Status != StatusApproved || stored.Comment != "已核实" {
		t.Errorf("stored = %+v err=%v, want APPROVED with comment", stored, err)
	}
	select {
	case ev := <-events:
		if ev.TicketID != id || ev.Status != StatusApproved {
			t.Errorf("wake event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no wake event after landing page confirm")
	}
}

func TestActionConfirmPostReplayRendersHandled(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	id := seedTicket(t, repo, clock)
	mux := newTestMux(repo, clock, bus)

	expire := clock.Now().Add(DefaultExpiry)
	secret := DeriveTicketSecret(testCallbackKey, id)
	form := url.Values{}
	form.Set("ticket_id", id)
	form.Set("decision", DecisionApprove)
	form.Set("expire", strconv.FormatInt(expire.Unix(), 10))
	form.Set("sig", SignCallback(secret, id, DecisionApprove, expire))
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, ActionPath, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	if w := post(); w.Code != http.StatusOK {
		t.Fatalf("first confirm = %d", w.Code)
	}
	w := post()
	if w.Code != http.StatusConflict {
		t.Fatalf("replay confirm = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "工单已被处理") {
		t.Errorf("replay page wrong:\n%s", w.Body.String())
	}
}

func TestActionConfirmPostBadSignature(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	bus := NewInMemoryEventBus()
	id := seedTicket(t, repo, clock)
	mux := newTestMux(repo, clock, bus)

	form := url.Values{}
	form.Set("ticket_id", id)
	form.Set("decision", DecisionApprove)
	form.Set("expire", strconv.FormatInt(clock.Now().Add(DefaultExpiry).Unix(), 10))
	form.Set("sig", strings.Repeat("0", 64))
	req := httptest.NewRequest(http.MethodPost, ActionPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	stored, _ := repo.Get(context.Background(), id)
	if stored.Status != StatusPending {
		t.Errorf("status = %q, want PENDING", stored.Status)
	}
}
