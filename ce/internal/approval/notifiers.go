// Notifier channel seam of the Approval Service (design/31 3.3.2/3.3.5,
// design/33 4.1, design/80 B-07). A Notifier delivers the approval card of a
// ticket to an IM channel; the caller supplies the two signed action URLs
// (SEC-13), so the notifier stays a pure transport. V1.0 ships WeCom
// (template_card) and DingTalk (actionCard) adapters plus the fan-out and
// retry wrappers (SEC-18: three attempts with exponential backoff of
// 1s/2s/4s, then the error is surfaced to the caller). Card copy has no
// emoji and no hardcoded identity claims (GAP-16, SEC-01).

package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Notifier delivers an approval card for a ticket. approveURL/rejectURL are
// the signed landing page links (BuildActionURL, SEC-13); the notifier must
// embed them verbatim.
type Notifier interface {
	SendApprovalCard(ctx context.Context, ticket *ApprovalTicket, approveURL, rejectURL string) error
}

// defaultNotifyClient is shared by notifiers that do not get an injected
// client; a 5s timeout keeps one slow webhook from wedging the sender.
var defaultNotifyClient = &http.Client{Timeout: 5 * time.Second}

// riskLabel renders the human-readable risk line of the card (design/33 4.1:
// "2（高危，须人工审批）").
func riskLabel(level int) string {
	switch level {
	case RiskRead:
		return "0（只读）"
	case RiskLow:
		return "1（低风险）"
	case RiskCritical:
		return "3（严重，须人工审批）"
	default:
		return "2（高危，须人工审批）"
	}
}

// cardArgs compacts the ticket arguments for card display.
func cardArgs(t *ApprovalTicket) string {
	if len(t.Arguments) == 0 || string(t.Arguments) == "null" {
		return "{}"
	}
	return string(t.Arguments)
}

// sendJSON posts the payload and validates both the HTTP status and the
// WeCom/DingTalk business ack (design/31 3.3.5: the PoC ignored the status
// code, which hid push failures).
func sendJSON(ctx context.Context, client *http.Client, webhookURL string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("approval: marshal notify payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("approval: build notify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c := client
	if c == nil {
		c = defaultNotifyClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("approval: notify send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("approval: notify rejected with HTTP %d", resp.StatusCode)
	}
	var ack struct {
		ErrCode *int `json:"errcode"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&ack); err == nil && ack.ErrCode != nil && *ack.ErrCode != 0 {
		return fmt.Errorf("approval: notify channel business error errcode=%d", *ack.ErrCode)
	}
	return nil
}

// WeComNotifier sends a WeCom template_card (button_interaction, design/33
// 4.1). Client may be nil to use the default 5s-timeout client; inject a
// custom one in tests.
type WeComNotifier struct {
	WebhookURL string
	Client     *http.Client
}

// NewWeComNotifier builds a WeCom notifier with the default HTTP client.
func NewWeComNotifier(webhookURL string) *WeComNotifier {
	return &WeComNotifier{WebhookURL: webhookURL}
}

// SendApprovalCard implements Notifier.
func (n *WeComNotifier) SendApprovalCard(ctx context.Context, ticket *ApprovalTicket, approveURL, rejectURL string) error {
	payload := map[string]any{
		"msgtype": "template_card",
		"template_card": map[string]any{
			"card_type": "button_interaction",
			"main_title": map[string]any{
				"title": "ADC 高危设备操作审批",
				"desc":  fmt.Sprintf("Agent 正在申请调度设备 %s", ticket.DeviceID),
			},
			"horizontal_content_list": []map[string]any{
				{"keyname": "目标设备", "value": ticket.DeviceID},
				{"keyname": "触发工具", "value": ticket.ToolName},
				{"keyname": "调用参数", "value": cardArgs(ticket)},
				{"keyname": "风险等级", "value": riskLabel(ticket.RiskLevel)},
				{"keyname": "申请来源", "value": ticket.AgentID},
			},
			"task_id": ticket.TicketID,
			"button_list": []map[string]any{
				{
					"text":  "核实并执行",
					"style": 1,
					"key":   "approve_" + ticket.TicketID,
					"url":   approveURL,
				},
				{
					"text":  "拦截终止",
					"style": 3,
					"key":   "reject_" + ticket.TicketID,
					"url":   rejectURL,
				},
			},
		},
	}
	return sendJSON(ctx, n.Client, n.WebhookURL, payload)
}

// DingTalkNotifier sends a DingTalk actionCard (design/33 4.1: the original
// 6.3 field set with signed action URLs plus the risk level and requester
// lines in the markdown text).
type DingTalkNotifier struct {
	WebhookURL string
	Client     *http.Client
}

// NewDingTalkNotifier builds a DingTalk notifier with the default client.
func NewDingTalkNotifier(webhookURL string) *DingTalkNotifier {
	return &DingTalkNotifier{WebhookURL: webhookURL}
}

// SendApprovalCard implements Notifier.
func (d *DingTalkNotifier) SendApprovalCard(ctx context.Context, ticket *ApprovalTicket, approveURL, rejectURL string) error {
	minutes := ticket.ExpireAt.Sub(ticket.CreatedAt).Round(time.Minute)
	if minutes < time.Minute {
		minutes = time.Minute
	}
	markdown := fmt.Sprintf("### ADC 设备操作审批请求\n\n"+
		"- **目标设备**: `%s`\n"+
		"- **触发工具**: `%s`\n"+
		"- **调用参数**: `%s`\n"+
		"- **风险等级**: `%s`\n"+
		"- **申请来源**: `%s`\n\n"+
		"> 请在 %d 分钟内完成核实，超时将自动拒绝。",
		ticket.DeviceID, ticket.ToolName, cardArgs(ticket), riskLabel(ticket.RiskLevel), ticket.AgentID,
		int(minutes/time.Minute))
	payload := map[string]any{
		"msgtype": "actionCard",
		"actionCard": map[string]any{
			"title":          "ADC 高危设备操作审批",
			"text":           markdown,
			"btnOrientation": "0",
			"btns": []map[string]string{
				{"title": "核实并执行", "actionURL": approveURL},
				{"title": "拦截终止", "actionURL": rejectURL},
			},
		},
	}
	return sendJSON(ctx, d.Client, d.WebhookURL, payload)
}

// MultiNotifier fans a card out to every configured channel (a tenant may
// use WeCom and DingTalk at the same time). Delivery failures are joined
// with errors.Join so a single broken channel does not hide the others.
type MultiNotifier struct {
	Channels []Notifier
}

// NewMultiNotifier builds a fan-out notifier, skipping nil channels.
func NewMultiNotifier(channels ...Notifier) *MultiNotifier {
	return &MultiNotifier{Channels: channels}
}

// SendApprovalCard implements Notifier.
func (m *MultiNotifier) SendApprovalCard(ctx context.Context, ticket *ApprovalTicket, approveURL, rejectURL string) error {
	var errs []error
	for _, n := range m.Channels {
		if n == nil {
			continue
		}
		if err := n.SendApprovalCard(ctx, ticket, approveURL, rejectURL); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("approval: %d notification channel(s) failed: %w", len(errs), errors.Join(errs...))
	}
	return nil
}

// RetryNotifier retries the wrapped notifier with exponential backoff
// (1s/2s/4s, design/31 3.3.5, SEC-18). After the attempts are exhausted the
// last error is returned so the caller can surface 12008 semantics; the
// ticket state machine itself is unaffected by push failures.
type RetryNotifier struct {
	Inner    Notifier
	Attempts int
	// Sleep is the context-aware backoff seam; nil uses a real
	// time.After-based sleep. Tests inject a recorder.
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewRetryNotifier wraps inner with 3 attempts and 1s/2s/4s backoffs.
func NewRetryNotifier(inner Notifier) *RetryNotifier {
	return &RetryNotifier{Inner: inner, Attempts: 3}
}

// SendApprovalCard implements Notifier.
func (r *RetryNotifier) SendApprovalCard(ctx context.Context, ticket *ApprovalTicket, approveURL, rejectURL string) error {
	attempts := r.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	var last error
	for i := 0; i < attempts; i++ {
		if last = r.Inner.SendApprovalCard(ctx, ticket, approveURL, rejectURL); last == nil {
			return nil
		}
		if i == attempts-1 {
			break
		}
		backoff := time.Duration(1<<i) * time.Second // 1s, 2s, 4s
		if r.Sleep != nil {
			if err := r.Sleep(ctx, backoff); err != nil {
				return fmt.Errorf("approval: notify backoff interrupted: %w", err)
			}
			continue
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("approval: notify backoff interrupted: %w", ctx.Err())
		case <-time.After(backoff):
		}
	}
	return fmt.Errorf("approval: notify failed after %d attempts: %w", attempts, last)
}
