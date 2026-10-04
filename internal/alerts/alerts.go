// Package alerts dispatches issue notifications to configured channels. Phase
// 2 ships a log sink, SSRF-guarded webhook, and signed automation webhook;
// richer channel types (Telegram, Slack, webhook routing) plug in behind the
// same Dispatch call.
package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bright-interaction/flare/internal/email"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/metrics"
	"github.com/bright-interaction/flare/internal/netguard"
)

// Channel is a destination for notifications, mapped from a stored
// notification_channels row. ID/OrgID identify the row so per-channel delivery
// status can be recorded (org-scoped); both may be empty for ad-hoc dispatch.
type Channel struct {
	ID     string
	OrgID  string
	Type   string
	Config json.RawMessage
}

// Notification is the payload describing the issue that fired the alert.
// Reason names what triggered it ("New issue", "Regression", "Spike: N events
// in Mm") and drives the subject/headline.
type Notification struct {
	ProjectID string `json:"-"`
	// Queued automation delivery keeps the same signed body across retries.
	// These fields never enter ordinary email/Slack/webhook payloads.
	ObservedAt      time.Time `json:"-"`
	AutomationEvent string    `json:"-"`
	ProjectName     string    `json:"project"`
	IssueID         string    `json:"issue_id"`
	Title           string    `json:"title"`
	Level           string    `json:"level"`
	Culprit         string    `json:"culprit"`
	EventCount      int64     `json:"events"`
	Reason          string    `json:"reason"`
	URL             string    `json:"url"`
}

type Dispatcher struct {
	client *http.Client
	mailer *email.Mailer
	// Recorder, when set, persists the outcome of each delivery attempt (nil
	// err = success). Wired by the server to the notification_channels delivery
	// columns so a silently-failing channel becomes visible. Kept as a hook so
	// the alerts package stays free of any DB dependency.
	Recorder func(ctx context.Context, orgID, channelID string, err error)
}

func NewDispatcher(mailer *email.Mailer) *Dispatcher {
	// SSRF guard: the Control hook runs after DNS resolution with the real
	// dialed IP, so it blocks loopback/private/link-local targets even under
	// DNS rebinding. Redirects are disabled.
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || isBlocked(ip) {
				return errors.New("blocked destination address")
			}
			return nil
		},
	}
	return &Dispatcher{
		mailer: mailer,
		client: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{DialContext: dialer.DialContext},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("redirects disabled")
			},
		},
	}
}

// Dispatch sends the notification to every channel, best-effort. Failures are
// recorded and logged, never propagated: alerting must not block or fail ingest.
//
// Channels are delivered CONCURRENTLY. Sequentially, a dispatch held its
// background worker slot for up to 10s per channel, so one org pointing two
// webhooks at a host that accepts the connection and never answers occupied a
// slot for 20s at a time; 24 slots is the whole estate's alert capacity, and
// every other tenant's alerts were then dropped with a log line, no retry and
// no queue. Concurrent delivery makes the hold time ONE timeout instead of N,
// which is the half of that fix that lives here. The other half is the per-org
// slot accounting in api.goBackgroundFor and the per-org channel cap.
func (d *Dispatcher) Dispatch(ctx context.Context, channels []Channel, n Notification) {
	var wg sync.WaitGroup
	for _, ch := range channels {
		wg.Add(1)
		go func(ch Channel) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("alert delivery panicked", "type", ch.Type, "panic", r)
				}
			}()
			err := d.send(ctx, ch, n)
			d.record(ctx, ch, err)
			if err != nil {
				slog.Warn("alert delivery failed", "type", ch.Type, "error", err)
			}
		}(ch)
	}
	wg.Wait()
}

// DispatchOne sends to a single channel and RETURNS the delivery result, so the
// "send test" operator action can show the real HTTP/SMTP outcome. The outcome
// is also recorded, so a test doubles as a live health check of the channel.
func (d *Dispatcher) DispatchOne(ctx context.Context, ch Channel, n Notification) error {
	err := d.send(ctx, ch, n)
	d.record(ctx, ch, err)
	return err
}

// send delivers to one channel and returns the delivery error (nil on success),
// with the destination URL reduced to scheme+host first.
//
// This wrapper is the whole point: every delivery error in this package reaches
// the caller through here, so DispatchOne, Dispatch and the Recorder all
// inherit the redaction and none of them can be the one that forgets.
//
// Go's *url.Error embeds the COMPLETE request URL. For a Slack incoming webhook
// the credential IS the path (/services/T0/B0/<secret>), and for a generic
// webhook it rides in the query. That error was persisted verbatim into
// notification_channels.last_error and served on GET /api/channels, which is a
// VIEWER route, so the least-privileged role in the org could read the org's
// Slack webhook URL and post into company Slack as Flare. redactChannelConfig
// and maskURL exist specifically to stop that URL being re-displayed; the error
// field sitting beside them in the same JSON object went around both.
//
// Note ai.Scrub is NOT sufficient here and is deliberately not used: it catches
// URL userinfo and key=value secrets, but a Slack webhook secret is a bare PATH
// segment with no key to match on. Dropping everything after the host is what
// makes this safe.
func (d *Dispatcher) send(ctx context.Context, ch Channel, n Notification) error {
	startedAt := time.Now().UTC()
	started := startedAt
	err := redactDeliveryError(d.deliver(ctx, ch, n))
	elapsed := time.Since(started)
	recordDeliveryMetric(ch, err, elapsed)
	outcome, statusClass, errorCode := integration.ClassifyError(err)
	receiptErr := integration.RecordResult(ctx, integration.Attempt{
		Scope:    integration.Scope{OrgID: ch.OrgID, ProjectID: n.ProjectID},
		Provider: "alerts", Operation: "dispatch", Channel: ch.Type,
		Outcome: outcome, StatusClass: statusClass, ErrorCode: errorCode,
		Elapsed: elapsed, AttemptedAt: startedAt, CompletedAt: time.Now().UTC(),
	})
	if receiptErr != nil {
		slog.Warn("connector receipt not accepted", "provider", "alerts", "operation", "dispatch", "receipt_status", integration.ReceiptStatus(receiptErr))
	}
	return err
}

// recordDeliveryMetric is deliberately process-wide and low-cardinality. The
// channel type is canonicalized before it becomes a label; destination URLs,
// tenant IDs, channel IDs, and notification content never enter the metrics
// registry. send is the shared boundary for Dispatch and DispatchOne, so each
// delivery attempt is observed exactly once.
func recordDeliveryMetric(ch Channel, err error, elapsed time.Duration) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	metrics.RecordIntegration("alerts", "dispatch", ch.Type, outcome, elapsed)
}

// redactDeliveryError rewrites any *url.Error in err's chain so only the method
// and scheme+host survive, keeping the underlying cause for diagnosis.
func redactDeliveryError(err error) error {
	if err == nil {
		return nil
	}
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		return err
	}
	dest := "the destination"
	if u, perr := url.Parse(uerr.URL); perr == nil && u.Host != "" {
		dest = u.Scheme + "://" + u.Host
	}
	cause := "delivery failed"
	if uerr.Err != nil {
		cause = uerr.Err.Error()
	}
	return fmt.Errorf("%s to %s failed: %s", uerr.Op, dest, cause)
}

// deliver dispatches to the channel implementation for ch.Type. Call send, not
// this: the error returned here still carries the full destination URL.
func (d *Dispatcher) deliver(ctx context.Context, ch Channel, n Notification) error {
	switch ch.Type {
	case "webhook":
		return d.webhook(ctx, ch.Config, n)
	case "automation_webhook":
		return d.automationWebhook(ctx, ch.Config, n)
	case "slack":
		return d.slack(ctx, ch.Config, n)
	case "email":
		return d.emailAlert(ch.Config, n)
	case "log":
		slog.Info("flare alert", "reason", n.Reason, "title", n.Title, "level", n.Level, "events", n.EventCount, "url", n.URL)
		return nil
	default:
		return fmt.Errorf("unsupported channel type %q", ch.Type)
	}
}

// automationWebhookPayload is the intentionally small contract sent to an
// external coding-loop worker. It carries identifiers and bounded counters,
// never issue text, stack traces, telemetry attributes, credentials, or model
// output. The receiver can use event to decide whether to fetch fresh Flare
// evidence before taking its own human-gated action.
type automationWebhookPayload struct {
	Schema    string `json:"schema"`
	Event     string `json:"event"`
	ProjectID string `json:"project_id"`
	// IssueID is empty for project/service watchdog notifications that are not
	// associated with a grouped issue. Keeping the field in the fixed schema
	// lets receivers use one decoder for issue and non-issue alerts.
	IssueID    string `json:"issue_id"`
	EventCount int64  `json:"event_count"`
	ObservedAt string `json:"observed_at"`
}

const (
	automationWebhookSchema        = "flare.automation.v1"
	maxAutomationWebhookEventCount = int64(1_000_000_000)
	automationWebhookReplayWindow  = 5 * time.Minute
)

func automationWebhookEvent(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "new issue":
		return "new_issue"
	case "regression":
		return "regression"
	case "anomaly":
		return "anomaly"
	case "silence":
		return "silence"
	case "monitor missing":
		return "monitor_missing"
	case "deploy failure":
		return "deploy_failure"
	case "deploy acceptance":
		return "deploy_acceptance"
	case "rollback":
		return "rollback"
	case "test":
		return "test"
	}
	lower := strings.ToLower(strings.TrimSpace(reason))
	for prefix, event := range map[string]string{
		"spike:": "spike", "connector ": "connector", "release ": "release",
		"service ": "service", "log ": "log", "usage ": "usage", "ai ": "ai",
	} {
		if strings.HasPrefix(lower, prefix) {
			return event
		}
	}
	return "alert"
}

// AutomationWebhookEvent is the canonical, bounded event vocabulary stored in
// the durable automation outbox. Raw alert reasons are never persisted there.
func AutomationWebhookEvent(reason string) string { return automationWebhookEvent(reason) }

func automationWebhookBody(n Notification, observedAt time.Time) ([]byte, error) {
	if strings.TrimSpace(n.ProjectID) == "" {
		return nil, errors.New("automation webhook requires a project identifier")
	}
	if n.EventCount < 0 || n.EventCount > maxAutomationWebhookEventCount {
		return nil, fmt.Errorf("automation webhook event count must be between 0 and %d", maxAutomationWebhookEventCount)
	}
	event := n.AutomationEvent
	if event == "" {
		event = automationWebhookEvent(n.Reason)
	}
	return json.Marshal(automationWebhookPayload{
		Schema: automationWebhookSchema, Event: event,
		ProjectID: n.ProjectID, IssueID: n.IssueID, EventCount: n.EventCount,
		ObservedAt: observedAt.UTC().Format(time.RFC3339Nano),
	})
}

func automationWebhookSignature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (d *Dispatcher) automationWebhook(ctx context.Context, cfgRaw json.RawMessage, n Notification) error {
	var cfg struct {
		URL           string `json:"url"`
		SigningSecret string `json:"signing_secret"`
	}
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil || cfg.URL == "" || cfg.SigningSecret == "" {
		return errors.New("automation webhook misconfigured (url and signing_secret are required)")
	}
	now := time.Now().UTC()
	observedAt := n.ObservedAt
	if observedAt.IsZero() {
		observedAt = now
	}
	body, err := automationWebhookBody(n, observedAt)
	if err != nil {
		return err
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Flare-Automation-Timestamp", timestamp)
	req.Header.Set("X-Flare-Automation-Signature", automationWebhookSignature(cfg.SigningSecret, timestamp, body))
	event := n.AutomationEvent
	if event == "" {
		event = automationWebhookEvent(n.Reason)
	}
	req.Header.Set("X-Flare-Automation-Event", event)
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("automation webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// record persists a delivery outcome when a recorder is wired and the channel
// has a stable id + org (ad-hoc dispatch may pass empty ids).
func (d *Dispatcher) record(ctx context.Context, ch Channel, err error) {
	if d.Recorder != nil && ch.ID != "" && ch.OrgID != "" {
		d.Recorder(ctx, ch.OrgID, ch.ID, err)
	}
}

// slack posts a Block Kit message to an incoming-webhook URL. Reuses the
// SSRF-guarded client (hooks.slack.com is public, loopback/private blocked).
func (d *Dispatcher) slack(ctx context.Context, cfgRaw json.RawMessage, n Notification) error {
	var cfg struct {
		WebhookURL string `json:"webhook_url"`
	}
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil || cfg.WebhookURL == "" {
		return errors.New("slack channel misconfigured (missing webhook_url)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.WebhookURL, bytes.NewReader(slackPayload(n)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// slackPayload renders the notification as a Slack Block Kit message.
func slackPayload(n Notification) []byte {
	reason := n.Reason
	if reason == "" {
		reason = "Alert"
	}
	meta := fmt.Sprintf("%s · %d event(s)", n.Level, n.EventCount)
	if n.Culprit != "" {
		meta += " · " + n.Culprit
	}
	blocks := []map[string]any{
		{"type": "section", "text": map[string]any{
			"type": "mrkdwn",
			"text": fmt.Sprintf("*%s* in *%s*\n<%s|%s>", slackEsc(reason), slackEsc(n.ProjectName), n.URL, slackEsc(n.Title)),
		}},
		{"type": "context", "elements": []map[string]any{
			{"type": "mrkdwn", "text": slackEsc(meta)},
		}},
	}
	// The fallback `text` is what Slack renders as the push and desktop
	// notification, i.e. the FIRST thing the on-call engineer sees, and it was
	// the one string here that skipped slackEsc. So an exception value of
	// "boom <!channel> <https://evil.example|Click here to reset your Flare
	// password>" paged the whole channel and showed an attacker-chosen
	// hyperlink with attacker-chosen label text, attributed to the Flare app,
	// while the blocks they only see after opening the message showed it
	// escaped. Anyone holding a project DSN key can write that string.
	//
	// Built from the already-escaped values rather than escaped again here, so
	// the fallback cannot drift from the blocks a second time.
	escReason, escTitle := slackEsc(reason), slackEsc(n.Title)
	b, _ := json.Marshal(map[string]any{
		"text":   fmt.Sprintf("[Flare] %s: %s", escReason, escTitle),
		"blocks": blocks,
	})
	return b
}

func slackEsc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}

// emailAlert renders and sends a new-issue notification to an email channel's
// recipient. Best-effort: a misconfigured channel or unconfigured mailer logs
// and returns, never blocking ingest.
func (d *Dispatcher) emailAlert(cfgRaw json.RawMessage, n Notification) error {
	if !d.mailer.Enabled() {
		return errors.New("email delivery not configured on this server (SMTP unset)")
	}
	var cfg struct {
		To string `json:"to"`
	}
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil || cfg.To == "" {
		return errors.New("email channel misconfigured (missing to)")
	}
	reason := n.Reason
	if reason == "" {
		reason = "Alert"
	}
	subject := fmt.Sprintf("[Flare] %s in %s: %s", reason, n.ProjectName, n.Title)
	text := fmt.Sprintf("%s in %s\n\n%s\n%s\nEvents: %d\n\nView: %s\n",
		reason, n.ProjectName, n.Title, n.Culprit, n.EventCount, n.URL)
	body := alertHTML(n, reason)
	return d.mailer.Send(cfg.To, subject, body, text)
}

func alertHTML(n Notification, reason string) string {
	esc := html.EscapeString
	culprit := ""
	if n.Culprit != "" {
		culprit = fmt.Sprintf(`<p style="margin:4px 0;color:#71717a;font:13px/1.5 ui-monospace,monospace">%s</p>`, esc(n.Culprit))
	}
	return fmt.Sprintf(`<div style="font:15px/1.6 -apple-system,Segoe UI,sans-serif;color:#18181b;max-width:560px">
<p style="margin:0 0 4px;font-size:12px;letter-spacing:.04em;text-transform:uppercase;color:#a16207">%s</p>
<h2 style="margin:0 0 2px;font-size:18px">%s</h2>
%s
<p style="margin:12px 0;color:#52525b">Project <strong>%s</strong> &middot; %d event(s) &middot; %s</p>
<p style="margin:16px 0"><a href="%s" style="display:inline-block;background:#f59e0b;color:#18181b;text-decoration:none;padding:9px 16px;border-radius:6px;font-weight:600">View issue</a></p>
</div>`, esc(reason), esc(n.Title), culprit, esc(n.ProjectName), n.EventCount, esc(n.Level), esc(n.URL))
}

func (d *Dispatcher) webhook(ctx context.Context, cfgRaw json.RawMessage, n Notification) error {
	var cfg struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil || cfg.URL == "" {
		return errors.New("webhook channel misconfigured (missing url)")
	}
	body, _ := json.Marshal(n)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}

func isBlocked(ip net.IP) bool {
	return netguard.IsBlockedIP(ip)
}
