package alerts

// Alert notification channel seam (FR-017 告警通道). The B2 slice ships a
// generic JSON webhook notifier (WeCom/DingTalk group-robot endpoints reuse
// the approval card webhook URLs), an email placeholder that logs the
// payload (SMTP delivery is out of scope), and the fan-out/retry wrappers
// mirroring internal/approval/notifiers.go (SEC-18: three attempts,
// 1s/2s/4s exponential backoff).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// AlertNotifier delivers one fired alert. The evaluator records the event
// in the history regardless of delivery outcome; failures are logged, not
// retried by the evaluator itself (the retry wrapper handles that).
type AlertNotifier interface {
	SendAlert(ctx context.Context, ev *Event) error
}

// defaultAlertClient is shared by notifiers without an injected client.
var defaultAlertClient = &http.Client{Timeout: 5 * time.Second}

// WebhookNotifier posts the alert payload as JSON to a generic webhook
// endpoint (group robot style). The payload is a flat object so any
// webhook bridge (WeCom/DingTalk/custom) can map it.
type WebhookNotifier struct {
	WebhookURL string
	Client     *http.Client
}

// NewWebhookNotifier builds a webhook notifier with the default client.
func NewWebhookNotifier(webhookURL string) *WebhookNotifier {
	return &WebhookNotifier{WebhookURL: webhookURL}
}

// alertPayload is the wire shape delivered to webhooks.
type alertPayload struct {
	AlertName string    `json:"alert_name"`
	RuleID    string    `json:"rule_id"`
	TenantID  string    `json:"tenant_id"`
	Severity  Severity  `json:"severity"`
	Metric    string    `json:"metric"`
	Operator  Operator  `json:"operator"`
	Threshold float64   `json:"threshold"`
	Observed  float64   `json:"observed"`
	FiredAt   time.Time `json:"fired_at"`
	Message   string    `json:"message"`
}

// payload renders the event into the webhook shape.
func payload(ev *Event) alertPayload {
	return alertPayload{
		AlertName: ev.RuleName,
		RuleID:    ev.RuleID,
		TenantID:  ev.TenantID,
		Severity:  ev.Severity,
		Metric:    ev.Metric,
		Operator:  ev.Operator,
		Threshold: ev.Threshold,
		Observed:  ev.Observed,
		FiredAt:   ev.FiredAt.UTC(),
		Message: fmt.Sprintf("ADC alert [%s] %s: %s %s %.4g (observed %.4g, tenant %s)",
			ev.Severity, ev.RuleName, ev.Metric, ev.Operator, ev.Threshold, ev.Observed, ev.TenantID),
	}
}

// SendAlert implements AlertNotifier.
func (n *WebhookNotifier) SendAlert(ctx context.Context, ev *Event) error {
	body, err := json.Marshal(payload(ev))
	if err != nil {
		return fmt.Errorf("alerts: marshal notify payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("alerts: build notify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c := n.Client
	if c == nil {
		c = defaultAlertClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("alerts: notify send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("alerts: notify rejected with HTTP %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// EmailNotifier is the FR-017 email channel placeholder: it logs the
// payload instead of sending mail. SMTP wiring (TLS, retry, deliverability)
// is a follow-up; the channel exists so the seam and the evaluator contract
// are stable from day one.
type EmailNotifier struct {
	Log *slog.Logger
}

// SendAlert implements AlertNotifier (logs only).
func (n *EmailNotifier) SendAlert(_ context.Context, ev *Event) error {
	log := n.Log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("alerts: email channel placeholder, alert logged instead of mailed",
		"rule_id", ev.RuleID, "severity", ev.Severity, "metric", ev.Metric, "observed", ev.Observed)
	return nil
}

// MultiAlertNotifier fans an alert out to every configured channel.
// Delivery failures are joined with errors.Join so one broken channel does
// not hide the others.
type MultiAlertNotifier struct {
	Channels []AlertNotifier
}

// NewMultiAlertNotifier builds a fan-out notifier, skipping nil channels.
func NewMultiAlertNotifier(channels ...AlertNotifier) *MultiAlertNotifier {
	return &MultiAlertNotifier{Channels: channels}
}

// SendAlert implements AlertNotifier.
func (m *MultiAlertNotifier) SendAlert(ctx context.Context, ev *Event) error {
	var errs []error
	for _, n := range m.Channels {
		if n == nil {
			continue
		}
		if err := n.SendAlert(ctx, ev); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("alerts: %d notification channel(s) failed: %w", len(errs), errors.Join(errs...))
	}
	return nil
}

// RetryAlertNotifier retries the wrapped notifier with exponential backoff
// (1s/2s/4s, SEC-18).
type RetryAlertNotifier struct {
	Inner    AlertNotifier
	Attempts int
	// Sleep is the context-aware backoff seam; nil uses time.After.
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewRetryAlertNotifier wraps inner with 3 attempts and 1s/2s/4s backoffs.
func NewRetryAlertNotifier(inner AlertNotifier) *RetryAlertNotifier {
	return &RetryAlertNotifier{Inner: inner, Attempts: 3}
}

// SendAlert implements AlertNotifier.
func (r *RetryAlertNotifier) SendAlert(ctx context.Context, ev *Event) error {
	attempts := r.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	var last error
	for i := 0; i < attempts; i++ {
		if last = r.Inner.SendAlert(ctx, ev); last == nil {
			return nil
		}
		if i == attempts-1 {
			break
		}
		backoff := time.Duration(1<<i) * time.Second // 1s, 2s, 4s
		if r.Sleep != nil {
			if err := r.Sleep(ctx, backoff); err != nil {
				return fmt.Errorf("alerts: notify backoff interrupted: %w", err)
			}
			continue
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("alerts: notify backoff interrupted: %w", ctx.Err())
		case <-time.After(backoff):
		}
	}
	return fmt.Errorf("alerts: notify failed after %d attempts: %w", attempts, last)
}
