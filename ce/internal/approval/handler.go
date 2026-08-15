// HTTP handlers of the Approval Service (design/33 3.4, design/31 3.3.3 and
// 3.3.4, design/80 B-07/F-11). POST /v1/hitl/callback is the formal JSON
// callback endpoint: the body carries the ticket id, the decision, the
// signature and the expiry the signature commits to (SEC-01/13); the
// decision lands through the repository CAS (SEC-11) and the suspended
// caller is woken through the event bus (SEC-10). GET/POST /v1/hitl/action
// renders the mobile approval landing page with its three states
// (success / already handled / expired, design/20 4.14); the confirm form
// posts the same signed payload back so a GET never mutates a ticket
// (SEC-01).

package approval

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"adc.dev/ce/internal/httpx"
)

// Callback and landing page paths (design/33 endpoint list 23/24).
const (
	CallbackPath = "/v1/hitl/callback"
	ActionPath   = "/v1/hitl/action"
)

// Callback decision errors, mapped to design/33 approval error codes by the
// handlers (12004 signature / 10001 invalid decision).
var (
	ErrCallbackSignature = errors.New("approval: callback signature verification failed")
	ErrBadDecision       = errors.New("approval: decision must be approve or reject")
)

// DeriveTicketSecret derives the ticket-level one-shot callback key from the
// platform callback key (ADC_HITL_CALLBACK_KEY, env-injected per SEC-13) and
// the ticket id. Deterministic derivation is what lets the callback handler
// verify card signatures after a restart and across processes: both sides
// only ever need the platform key, and the derived key is never logged or
// persisted (design/33 1.4 keeps the database hash-only).
func DeriveTicketSecret(callbackKey, ticketID string) string {
	mac := hmac.New(sha256.New, []byte(callbackKey))
	_, _ = mac.Write([]byte("adc-hitl-ticket-secret-v1"))
	_, _ = mac.Write([]byte{0x1f})
	_, _ = mac.Write([]byte(ticketID))
	return hex.EncodeToString(mac.Sum(nil))
}

// BuildActionURL builds a signed approval landing page URL (design/33 1.4):
// /v1/hitl/action with ticket_id, decision, expire (Unix seconds) and
// sig = SignCallback(secret, ticketID, decision, expire) (SEC-13).
func BuildActionURL(baseURL, ticketID, decision string, expire time.Time, secret string) string {
	q := url.Values{}
	q.Set("ticket_id", ticketID)
	q.Set("decision", decision)
	q.Set("expire", strconv.FormatInt(expire.Unix(), 10))
	q.Set("sig", SignCallback(secret, ticketID, decision, expire))
	return strings.TrimRight(baseURL, "/") + ActionPath + "?" + q.Encode()
}

// CallbackRequest is the signed callback payload (design/33 3.4.1). The
// signature commits to ticket_id, decision and expire (Unix seconds);
// approver and comment are optional decision metadata.
type CallbackRequest struct {
	TicketID  string `json:"ticket_id"`
	Decision  string `json:"decision"`
	Signature string `json:"signature"`
	Expire    int64  `json:"expire"`
	Approver  string `json:"approver,omitempty"`
	Comment   string `json:"comment,omitempty"`
}

// CallbackHandler serves the HITL callback and landing page endpoints.
type CallbackHandler struct {
	Repo TicketRepo
	Bus  TicketEventBus

	// CallbackKey is the platform callback signing key (SEC-13); the
	// per-ticket verification key is derived from it.
	CallbackKey string

	// Now is the clock seam; nil means time.Now (tests inject a fake).
	Now func() time.Time
}

// NewCallbackHandler assembles a handler over the given repo and wake bus.
func NewCallbackHandler(repo TicketRepo, bus TicketEventBus, callbackKey string) *CallbackHandler {
	return &CallbackHandler{Repo: repo, Bus: bus, CallbackKey: callbackKey}
}

// Routes registers the callback and landing page endpoints on mux. The
// formal endpoints are always registered (design/80 B-07); dev-only
// shortcuts live in cmd.
func (h *CallbackHandler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+CallbackPath, h.handleCallbackJSON)
	mux.HandleFunc("GET "+ActionPath, h.handleActionPage)
	mux.HandleFunc("POST "+ActionPath, h.handleActionConfirm)
}

func (h *CallbackHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// decide is the shared decision core: validate the decision enum, verify the
// signature derived from the platform callback key (SEC-13), enforce the
// signed time window (SEC-11), and land the decision through the repository
// CAS. On success the wake event is published (SEC-10); the decision is
// committed by then, so a wake transport failure is best-effort only.
func (h *CallbackHandler) decide(ctx context.Context, req CallbackRequest) (*ApprovalTicket, error) {
	status, ok := DecisionStatus(req.Decision)
	if !ok {
		return nil, ErrBadDecision
	}
	expire := time.Unix(req.Expire, 0).UTC()
	secret := DeriveTicketSecret(h.CallbackKey, req.TicketID)
	if !VerifyCallbackSignature(secret, req.TicketID, req.Decision, req.Signature, expire) {
		return nil, ErrCallbackSignature
	}
	if !CallbackWindowValid(expire, h.now()) {
		return nil, ErrTicketExpired
	}
	cur, err := h.Repo.Get(ctx, req.TicketID)
	if err != nil {
		return nil, err
	}
	updated, err := h.Repo.Transition(ctx, req.TicketID, cur.Version, TransitionCmd{
		Status:   status,
		Approver: req.Approver,
		Comment:  req.Comment,
	})
	if err != nil {
		return nil, err
	}
	if h.Bus != nil {
		_ = h.Bus.PublishResolved(ctx, updated.TicketID, updated.Status)
	}
	return updated, nil
}

// handleCallbackJSON is the formal callback endpoint (design/33 3.4.1).
func (h *CallbackHandler) handleCallbackJSON(w http.ResponseWriter, r *http.Request) {
	var req CallbackRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "10001", "invalid json", httpx.TraceIDFrom(r))
		return
	}
	if req.TicketID == "" || req.Signature == "" || req.Expire == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "10001", "ticket_id, signature and expire are required", httpx.TraceIDFrom(r))
		return
	}
	updated, err := h.decide(r.Context(), req)
	if err != nil {
		h.writeDecideError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"ticket_id": updated.TicketID,
		"status":    apiStatus(updated.Status),
	})
}

// writeDecideError maps the decision core's sentinel errors to the design/33
// approval error codes (12004/12001/12002/12003) and HTTP statuses.
func (h *CallbackHandler) writeDecideError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrCallbackSignature):
		httpx.WriteError(w, http.StatusUnauthorized, "12004", "callback signature verification failed", httpx.TraceIDFrom(r))
	case errors.Is(err, ErrBadDecision):
		httpx.WriteError(w, http.StatusBadRequest, "10001", "decision must be approve or reject", httpx.TraceIDFrom(r))
	case errors.Is(err, ErrTicketNotFound):
		httpx.WriteError(w, http.StatusNotFound, "12001", "ticket not found", httpx.TraceIDFrom(r))
	case errors.Is(err, ErrTicketExpired):
		httpx.WriteError(w, http.StatusForbidden, "12003", "ticket expired", httpx.TraceIDFrom(r))
	case errors.Is(err, ErrTicketHandled):
		httpx.WriteError(w, http.StatusConflict, "12002", "ticket already handled", httpx.TraceIDFrom(r))
	default:
		httpx.WriteError(w, http.StatusInternalServerError, "10007", "internal error", httpx.TraceIDFrom(r))
	}
}

// apiStatus renders the wire status of design/33 3.4.1 (lowercase).
func apiStatus(s Status) string {
	switch s {
	case StatusApproved:
		return "approved"
	case StatusRejected:
		return "rejected"
	default:
		return strings.ToLower(string(s))
	}
}

// --- landing page (design/20 4.14, F-11) ---

// actionRequestFrom parses the signed action parameters shared by the GET
// query and the confirm form POST.
func actionRequestFrom(ticketID, decision, expireRaw, sig string) (CallbackRequest, time.Time, error) {
	req := CallbackRequest{TicketID: ticketID, Decision: decision, Signature: sig}
	if req.TicketID == "" || req.Signature == "" {
		return req, time.Time{}, fmt.Errorf("approval: missing action parameters")
	}
	expireUnix, err := strconv.ParseInt(expireRaw, 10, 64)
	if err != nil || expireUnix <= 0 {
		return req, time.Time{}, fmt.Errorf("approval: invalid expire parameter")
	}
	return req, time.Unix(expireUnix, 0).UTC(), nil
}

// handleActionPage renders the landing page for a card button click: verify
// the URL signature, then show the confirm form for a still-PENDING ticket
// or one of the failure/expired states. A GET never mutates a ticket
// (SEC-01: the PoC's GET decided tickets directly).
func (h *CallbackHandler) handleActionPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req, expire, err := actionRequestFrom(q.Get("ticket_id"), q.Get("decision"), q.Get("expire"), q.Get("sig"))
	if err != nil {
		renderActionPage(w, http.StatusBadRequest, invalidActionPage("链接无效", "审批链接参数缺失或不完整"))
		return
	}
	if _, ok := DecisionStatus(req.Decision); !ok {
		renderActionPage(w, http.StatusBadRequest, invalidActionPage("链接无效", "审批链接参数缺失或不完整"))
		return
	}
	secret := DeriveTicketSecret(h.CallbackKey, req.TicketID)
	if !VerifyCallbackSignature(secret, req.TicketID, req.Decision, req.Signature, expire) {
		// design/20 4.14: a failed signature must not reveal ticket data.
		renderActionPage(w, http.StatusUnauthorized, invalidActionPage("签名校验失败", "该审批链接无效或已被篡改"))
		return
	}
	ticket, err := h.Repo.Get(r.Context(), req.TicketID)
	if err != nil {
		renderActionPage(w, http.StatusNotFound, invalidActionPage("工单不存在", "未找到对应的审批工单"))
		return
	}
	if !CallbackWindowValid(expire, h.now()) {
		renderActionPage(w, http.StatusForbidden, expiredActionPage(ticket, expire))
		return
	}
	if ticket.Status != StatusPending {
		renderActionPage(w, http.StatusConflict, handledActionPage(ticket))
		return
	}
	renderActionPage(w, http.StatusOK, confirmActionPage(ticket, req, expire))
}

// handleActionConfirm is the landing page confirm form target. It performs
// the same verified decision as the JSON callback and renders the success /
// failure / expired states.
func (h *CallbackHandler) handleActionConfirm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		renderActionPage(w, http.StatusBadRequest, invalidActionPage("链接无效", "表单解析失败"))
		return
	}
	req, expire, err := actionRequestFrom(
		r.PostForm.Get("ticket_id"), r.PostForm.Get("decision"),
		r.PostForm.Get("expire"), r.PostForm.Get("sig"))
	if err != nil {
		renderActionPage(w, http.StatusBadRequest, invalidActionPage("链接无效", "审批链接参数缺失或不完整"))
		return
	}
	req.Expire = expire.Unix()
	req.Comment = r.PostForm.Get("comment")
	updated, err := h.decide(r.Context(), req)
	switch {
	case errors.Is(err, ErrBadDecision):
		renderActionPage(w, http.StatusBadRequest, invalidActionPage("链接无效", "审批链接参数缺失或不完整"))
	case errors.Is(err, ErrCallbackSignature):
		renderActionPage(w, http.StatusUnauthorized, invalidActionPage("签名校验失败", "该审批链接无效或已被篡改"))
	case errors.Is(err, ErrTicketNotFound):
		renderActionPage(w, http.StatusNotFound, invalidActionPage("工单不存在", "未找到对应的审批工单"))
	case errors.Is(err, ErrTicketExpired):
		renderActionPage(w, http.StatusForbidden, expiredActionPage(nil, expire))
	case errors.Is(err, ErrTicketHandled):
		ticket, _ := h.Repo.Get(r.Context(), req.TicketID)
		renderActionPage(w, http.StatusConflict, handledActionPage(ticket))
	case err != nil:
		renderActionPage(w, http.StatusInternalServerError, invalidActionPage("处理失败", "系统繁忙，请稍后再试"))
	default:
		renderActionPage(w, http.StatusOK, successActionPage(updated))
	}
}

// --- page builders ---

type actionPage struct {
	tone     string // confirm / success / failure / expired / invalid
	title    string
	headline string
	rows     [][2]string
	note     string
	form     *actionConfirmForm
}

type actionConfirmForm struct {
	ticketID  string
	decision  string
	signature string
	expire    int64
	action    string
	reject    bool
}

// markerSymbol is a plain glyph inside the colored circle (cards and pages
// carry no emoji, GAP-16).
func markerSymbol(tone string) string {
	switch tone {
	case "success":
		return "OK"
	case "confirm":
		return "?"
	default:
		return "i"
	}
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// renderActionPage writes a minimal mobile-first HTML page (375px single
// column, design/20 4.14). Every dynamic value is HTML-escaped.
func renderActionPage(w http.ResponseWriter, status int, p actionPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	colors := map[string]string{
		"confirm": "#2563eb",
		"success": "#16a34a",
		"failure": "#d97706",
		"expired": "#6b7280",
		"invalid": "#dc2626",
	}
	color := colors[p.tone]
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(`<title>` + html.EscapeString(p.title) + `</title><style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0;padding:24px 16px;background:#f3f4f6;color:#111827}
.card{max-width:375px;margin:0 auto;background:#fff;border-radius:12px;padding:24px;box-shadow:0 1px 3px rgba(0,0,0,.1)}
.marker{width:56px;height:56px;border-radius:50%;margin:0 auto 16px;display:flex;align-items:center;justify-content:center;font-size:22px;font-weight:700;color:#fff}
h1{font-size:20px;text-align:center;margin:0 0 8px}
.rows{border-top:1px solid #e5e7eb;margin:16px 0}
.row{display:flex;justify-content:space-between;padding:10px 0;border-bottom:1px solid #f3f4f6;font-size:14px}
.row .k{color:#6b7280;flex-shrink:0}
.row .v{max-width:62%;text-align:right;word-break:break-all;font-family:ui-monospace,Menlo,monospace;font-size:13px}
.note{font-size:13px;color:#6b7280;text-align:center;margin:0 0 16px}
.btn{display:block;width:100%;padding:14px;border:0;border-radius:8px;font-size:16px;color:#fff;cursor:pointer}
.link{display:block;text-align:center;font-size:13px;color:#2563eb;margin-top:16px;text-decoration:none}
textarea{width:100%;box-sizing:border-box;border:1px solid #d1d5db;border-radius:8px;padding:10px;font-size:14px;margin-bottom:12px;font-family:inherit}
</style></head><body><div class="card">`)
	fmt.Fprintf(&b, `<div class="marker" style="background:%s">%s</div>`, color, markerSymbol(p.tone))
	fmt.Fprintf(&b, `<h1>%s</h1>`, html.EscapeString(p.headline))
	if len(p.rows) > 0 {
		b.WriteString(`<div class="rows">`)
		for _, row := range p.rows {
			fmt.Fprintf(&b, `<div class="row"><span class="k">%s</span><span class="v">%s</span></div>`,
				html.EscapeString(row[0]), html.EscapeString(row[1]))
		}
		b.WriteString(`</div>`)
	}
	if p.note != "" {
		fmt.Fprintf(&b, `<p class="note">%s</p>`, html.EscapeString(p.note))
	}
	if p.form != nil {
		fmt.Fprintf(&b, `<form method="post" action="%s">`, ActionPath)
		fmt.Fprintf(&b, `<input type="hidden" name="ticket_id" value="%s">`, html.EscapeString(p.form.ticketID))
		fmt.Fprintf(&b, `<input type="hidden" name="decision" value="%s">`, html.EscapeString(p.form.decision))
		fmt.Fprintf(&b, `<input type="hidden" name="signature" value="%s">`, html.EscapeString(p.form.signature))
		fmt.Fprintf(&b, `<input type="hidden" name="expire" value="%d">`, p.form.expire)
		if p.form.reject {
			b.WriteString(`<textarea name="comment" rows="2" placeholder="审批意见（可选）"></textarea>`)
		}
		fmt.Fprintf(&b, `<button class="btn" style="background:%s" type="submit">%s</button>`, color, html.EscapeString(p.form.action))
		b.WriteString(`</form>`)
	}
	b.WriteString(`<a class="link" href="/console/approvals">返回控制台查看工单</a>`)
	b.WriteString(`</div></body></html>`)
	_, _ = w.Write([]byte(b.String()))
}

func ticketRows(t *ApprovalTicket) [][2]string {
	if t == nil {
		return nil
	}
	return [][2]string{
		{"工单号", t.TicketID},
		{"目标设备", t.DeviceID},
		{"触发工具", t.ToolName},
	}
}

// confirmActionPage renders the final confirmation step for a still-PENDING
// ticket (design/20 4.14: the confirm click, not the card button, performs
// the decision).
func confirmActionPage(t *ApprovalTicket, req CallbackRequest, expire time.Time) actionPage {
	action := "确认拒绝"
	reject := true
	if req.Decision == DecisionApprove {
		action = "确认执行"
		reject = false
	}
	rows := append(ticketRows(t), [2]string{"调用参数", cardArgs(t)}, [2]string{"风险等级", riskLabel(t.RiskLevel)}, [2]string{"到期时间", fmtTime(expire)})
	return actionPage{
		tone:     "confirm",
		title:    "ADC 审批确认",
		headline: "审批确认",
		rows:     rows,
		note:     "请在到期前完成确认，超时将自动拒绝",
		form: &actionConfirmForm{
			ticketID:  req.TicketID,
			decision:  req.Decision,
			signature: req.Signature,
			expire:    expire.Unix(),
			action:    action,
			reject:    reject,
		},
	}
}

// successActionPage renders the success state after a confirmed decision
// (design/20 4.14).
func successActionPage(t *ApprovalTicket) actionPage {
	headline := "已确认执行"
	note := "指令已下发，结果回传 Agent 中"
	if t.Status == StatusRejected {
		headline = "已拒绝执行"
		note = "该指令未执行"
	}
	rows := append(ticketRows(t), [2]string{"处理时间", fmtTime(orNow(t.DecidedAt))})
	return actionPage{tone: "success", title: "ADC 审批完成", headline: headline, rows: rows, note: note}
}

// handledActionPage renders the failure state for an already-decided ticket
// (design/20 4.14 shows the handler, time and outcome).
func handledActionPage(t *ApprovalTicket) actionPage {
	rows := ticketRows(t)
	if t != nil {
		result := map[Status]string{
			StatusApproved: "已通过",
			StatusRejected: "已拒绝",
			StatusExpired:  "已失效",
		}[t.Status]
		rows = append(rows,
			[2]string{"处理人", t.Approver},
			[2]string{"处理时间", fmtTime(orNow(t.DecidedAt))},
			[2]string{"处理结果", result},
		)
	}
	return actionPage{
		tone:     "failure",
		title:    "ADC 审批工单",
		headline: "工单已被处理",
		rows:     rows,
		note:     "如有疑问请联系管理员",
	}
}

// expiredActionPage renders the expired state (design/20 4.14).
func expiredActionPage(t *ApprovalTicket, expire time.Time) actionPage {
	rows := ticketRows(t)
	rows = append(rows, [2]string{"超时时间", fmtTime(expire)})
	return actionPage{
		tone:     "expired",
		title:    "ADC 审批工单",
		headline: "工单已超时失效",
		rows:     rows,
		note:     "已自动拒绝，指令未执行",
	}
}

// invalidActionPage is the generic failure page used for signature failures
// and unknown tickets; it deliberately carries no ticket data (design/20).
func invalidActionPage(headline, note string) actionPage {
	return actionPage{tone: "invalid", title: "ADC 审批工单", headline: headline, note: note}
}

func orNow(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
