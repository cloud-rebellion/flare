package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/bright-interaction/flare/internal/alerts"
	"github.com/bright-interaction/flare/internal/automationdelivery"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/netguard"
)

type channelResponse struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	Config        json.RawMessage `json:"config"`
	Enabled       bool            `json:"enabled"`
	LastAttemptAt *string         `json:"last_attempt_at"`
	LastOkAt      *string         `json:"last_ok_at"`
	LastError     string          `json:"last_error"`
	// ProjectIDs is the routing list. EMPTY MEANS ALL PROJECTS, which is both
	// the pre-routing behaviour and the default, so an existing channel keeps
	// receiving everything until someone narrows it deliberately.
	ProjectIDs []string `json:"project_ids"`
}

// toChannelResponse maps a stored channel row to the API shape, redacting the
// secret and surfacing the delivery-health columns so the UI can show whether
// the channel last succeeded or failed (and why).
func (s *Server) toChannelResponse(c *generated.NotificationChannel) channelResponse {
	return channelResponse{
		ID:            c.ID,
		Type:          c.Type,
		Config:        s.redactChannelConfig(c.Type, c.Config),
		Enabled:       c.Enabled,
		LastAttemptAt: tsPtr(c.LastAttemptAt),
		LastOkAt:      tsPtr(c.LastOkAt),
		LastError:     textStr(c.LastError),
	}
}

// channelSecretField returns the primary config key holding a bearer secret (a
// Slack incoming-webhook URL or a generic webhook URL - whoever holds it can
// post into the org's channel), or "" for channel types with no URL secret.
// automation_webhook has a second signing_secret key; channelSecretFields
// includes both values for encryption and redaction.
func channelSecretField(chType string) string {
	switch chType {
	case "webhook":
		return "url"
	case "slack":
		return "webhook_url"
	case "automation_webhook":
		return "url"
	}
	return ""
}

func channelSecretFields(chType string) []string {
	if chType == "automation_webhook" {
		return []string{"url", "signing_secret"}
	}
	if field := channelSecretField(chType); field != "" {
		return []string{field}
	}
	return nil
}

// mapChannelSecret applies fn to the secret-bearing field of a channel config
// and returns the rewritten JSON. On any parse error it returns the input
// unchanged so a malformed row never breaks dispatch.
func mapChannelSecret(chType string, cfg json.RawMessage, fn func(string) string, redactSigningSecret bool) json.RawMessage {
	fields := channelSecretFields(chType)
	if len(fields) == 0 {
		return cfg
	}
	var m map[string]any
	if json.Unmarshal(cfg, &m) != nil {
		return cfg
	}
	for _, field := range fields {
		if v, ok := m[field].(string); ok && v != "" {
			// A signing secret is not a URL. Revealing even its suffix in a
			// viewer-facing channel projection would give an attacker a useful
			// comparison oracle, so redact it completely while preserving the
			// generic URL masking for the destination itself.
			if redactSigningSecret && chType == "automation_webhook" && field == "signing_secret" {
				m[field] = "[REDACTED]"
				continue
			}
			m[field] = fn(v)
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return cfg
	}
	return b
}

// encryptChannelConfig / decryptChannelConfig encrypt the webhook secret at rest
// so a DB dump does not expose it; decryption happens only at the dispatch
// boundary just before the alert is sent.
func (s *Server) encryptChannelConfig(chType string, cfg json.RawMessage) json.RawMessage {
	return mapChannelSecret(chType, cfg, s.secrets.Encrypt, false)
}

// decryptChannelConfig opens the stored webhook secret at the dispatch
// boundary. An undecryptable value becomes EMPTY rather than the ciphertext:
// posting "enc:v1:<base64>" at a third-party webhook host both leaks the
// encrypted credential and looks like a working delivery. Empty makes the send
// fail, which is what a broken key should look like.
func (s *Server) decryptChannelConfig(chType string, cfg json.RawMessage) json.RawMessage {
	return mapChannelSecret(chType, cfg, func(v string) string {
		out, err := s.secrets.Decrypt(v)
		if err != nil {
			slog.Error("alert channel secret cannot be decrypted; check FLARE_SECRET_KEY", "error", err)
			return ""
		}
		return out
	}, false)
}

// redactChannelConfig decrypts then masks the webhook secret before it is
// returned by any read handler: never re-displayed in full, only host plus a
// short suffix for identification, mirroring the "shown once" rule for API keys
// and the GitHub token. Email destinations and the empty log config are kept.
func (s *Server) redactChannelConfig(chType string, cfg json.RawMessage) json.RawMessage {
	return mapChannelSecret(chType, cfg, func(v string) string {
		out, err := s.secrets.Decrypt(v)
		if err != nil {
			return "(unreadable: check FLARE_SECRET_KEY)"
		}
		return maskURL(out)
	}, true)
}

// maskURL keeps scheme://host and a 4-char suffix, dropping the secret path so
// the value identifies the endpoint without being replayable.
func maskURL(raw string) string {
	scheme, host := "", raw
	if i := strings.Index(raw, "://"); i >= 0 {
		scheme = raw[:i+3]
		host = raw[i+3:]
	}
	if j := strings.IndexByte(host, '/'); j >= 0 {
		host = host[:j]
	}
	tail := ""
	if len(raw) >= 4 {
		tail = raw[len(raw)-4:]
	}
	return scheme + host + "/..." + tail
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type   string          `json:"type"`
		Config json.RawMessage `json:"config"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch req.Type {
	case "log":
		if len(req.Config) == 0 {
			req.Config = json.RawMessage(`{}`)
		}
	case "webhook":
		var cfg struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(req.Config, &cfg) != nil {
			writeErr(w, http.StatusBadRequest, "webhook channel requires config.url (https)")
			return
		}
		// strings.HasPrefix(cfg.URL, "http") accepted "http://169.254.169.254"
		// and, being a prefix test, "httpfoo" too. The dial guard blocks the
		// delivery, so what shipped was a channel the UI shows as configured
		// whose every alert fails silently.
		if err := netguard.ValidatePublicURL(cfg.URL); err != nil {
			writeErr(w, http.StatusBadRequest, "webhook config.url "+err.Error())
			return
		}
	case "slack":
		var cfg struct {
			WebhookURL string `json:"webhook_url"`
		}
		if json.Unmarshal(req.Config, &cfg) != nil {
			writeErr(w, http.StatusBadRequest, "slack channel requires config.webhook_url (https incoming webhook)")
			return
		}
		if err := netguard.ValidatePublicURL(cfg.WebhookURL); err != nil {
			writeErr(w, http.StatusBadRequest, "slack config.webhook_url "+err.Error())
			return
		}
	case "automation_webhook":
		var cfg struct {
			URL           string `json:"url"`
			SigningSecret string `json:"signing_secret"`
		}
		if json.Unmarshal(req.Config, &cfg) != nil || strings.TrimSpace(cfg.URL) == "" || strings.TrimSpace(cfg.SigningSecret) == "" {
			writeErr(w, http.StatusBadRequest, "automation_webhook requires config.url and config.signing_secret")
			return
		}
		if _, err := automationdelivery.ConfiguredMode(req.Config); err != nil {
			writeErr(w, http.StatusBadRequest, "automation_webhook receipt_mode must be http or reactor-v1")
			return
		}
		if err := netguard.ValidatePublicURL(strings.TrimSpace(cfg.URL)); err != nil {
			writeErr(w, http.StatusBadRequest, "automation_webhook config.url "+err.Error())
			return
		}
		if len([]byte(cfg.SigningSecret)) < 16 || len([]byte(cfg.SigningSecret)) > 256 || strings.IndexFunc(cfg.SigningSecret, unicode.IsControl) >= 0 {
			writeErr(w, http.StatusBadRequest, "automation_webhook config.signing_secret must be 16-256 printable bytes")
			return
		}
	case "email":
		var cfg struct {
			To string `json:"to"`
		}
		if json.Unmarshal(req.Config, &cfg) != nil || !strings.Contains(cfg.To, "@") {
			writeErr(w, http.StatusBadRequest, "email channel requires a valid config.to address")
			return
		}
		if !s.mailer.Enabled() {
			writeErr(w, http.StatusBadRequest, "email delivery is not configured on this server (set SMTP_*)")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "type must be email, slack, webhook, automation_webhook, or log")
		return
	}

	// Cap the channels an org may hold. Every enabled channel is one more
	// outbound delivery per alert, each holding a background slot for up to its
	// own timeout, so "how many channels can one tenant create" was directly
	// "how much of the shared alert pool can one tenant occupy". Nothing capped
	// it: CountEnabledNotificationChannelsByOrg existed and only the dashboard
	// read it.
	org := orgIDFrom(r.Context())
	if n, cerr := s.q.CountEnabledNotificationChannelsByOrg(r.Context(), org); cerr == nil && n >= maxChannelsPerOrg {
		writeErr(w, http.StatusConflict, "this workspace already has the maximum number of notification channels")
		return
	} else if cerr != nil {
		slogError(w, "count channels", cerr)
		return
	}

	ch, err := s.q.CreateNotificationChannel(r.Context(), generated.CreateNotificationChannelParams{
		ID: id.New(), OrgID: org, Type: req.Type, Config: s.encryptChannelConfig(req.Type, req.Config), Enabled: true,
	})
	if err != nil {
		slogError(w, "create channel", err)
		return
	}
	// Audited, because creating a channel is how an alert stream is redirected.
	// channel.update was audited and create and delete were not, which is the
	// one operation on this resource that IS recorded being the least
	// sensitive: a compromised member could point a new webhook at their own
	// endpoint, mirror every alert in the org (including the
	// sensitive-data-in-payload security events), and the audit log an admin
	// reads showed nothing. Deleting it afterwards was equally invisible.
	s.audit(r.Context(), "channel.create", ch.ID+" ("+ch.Type+")")
	writeJSON(w, http.StatusCreated, s.toChannelResponse(ch))
}

// maxChannelsPerOrg bounds the fan-out of one alert. Well above any real
// workspace (a handful of Slack channels, a webhook, a couple of mailboxes).
const maxChannelsPerOrg = 25

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	chans, err := s.q.ListNotificationChannelsByOrg(r.Context(), orgIDFrom(r.Context()))
	if err != nil {
		slogError(w, "list channels", err)
		return
	}
	out := make([]channelResponse, 0, len(chans))
	for _, c := range chans {
		resp := s.toChannelResponse(c)
		// Routing, so the UI can show and edit it. A read error here must NOT
		// degrade to empty: empty renders as "all projects", so a transient
		// failure would display a narrowed channel as an estate-wide broadcast,
		// and a subsequent save would then persist that wider set. Fail closed
		// (500) rather than show a channel as broader than it really is.
		pids, err := s.q.ListChannelProjects(r.Context(), c.ID)
		if err != nil {
			slogError(w, "list channel routing", err)
			return
		}
		resp.ProjectIDs = pids
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTestChannel sends a synthetic notification to one channel and returns
// the real delivery outcome, so an operator can prove a channel actually
// reaches a human before relying on it. The attempt is recorded like any real
// delivery, so a test also refreshes the channel's health status.
func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	if !s.testLimiter.Allow("test:" + orgIDFrom(r.Context())) {
		writeErr(w, http.StatusTooManyRequests, "too many test sends, wait a moment")
		return
	}
	ch, err := s.q.GetNotificationChannel(r.Context(), generated.GetNotificationChannelParams{
		ID: chi.URLParam(r, "id"), OrgID: orgIDFrom(r.Context()),
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	n := alerts.Notification{
		ProjectName: "Flare",
		Title:       "Test notification from Flare",
		Level:       "info",
		Reason:      "Test",
		EventCount:  1,
		URL:         strings.TrimRight(s.cfg.BaseURL, "/") + "/settings",
	}
	if ch.Type == "automation_webhook" {
		// The signed automation envelope requires a real project. Require the
		// operator to select one that belongs to this workspace and is routed
		// to this channel; never invent a project or let a channel test bypass
		// the project's routing boundary.
		var req struct {
			ProjectID string `json:"project_id"`
		}
		if decodeJSON(r, &req) != nil || strings.TrimSpace(req.ProjectID) == "" {
			writeErr(w, http.StatusBadRequest, "project_id is required for an automation webhook test")
			return
		}
		project, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{
			ID: req.ProjectID, OrgScope: ch.OrgID,
		})
		if err != nil {
			writeErr(w, http.StatusNotFound, "project not found")
			return
		}
		routed, err := s.q.ListChannelProjects(r.Context(), ch.ID)
		if err != nil {
			slogError(w, "get automation test channel routing", err)
			return
		}
		if len(routed) > 0 && !slices.Contains(routed, project.ID) {
			writeErr(w, http.StatusBadRequest, "channel is not routed to the selected project")
			return
		}
		n.ProjectID = project.ID
		n.ProjectName = project.Name
		n.URL = strings.TrimRight(s.cfg.BaseURL, "/") + "/projects/" + project.ID
	}
	if derr := s.dispatcher.DispatchOne(r.Context(), alerts.Channel{
		ID: ch.ID, OrgID: ch.OrgID, Type: ch.Type, Config: s.decryptChannelConfig(ch.Type, ch.Config),
	}, n); derr != nil {
		s.audit(r.Context(), "channel.test", ch.ID+" (failed)")
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": derr.Error()})
		return
	}
	s.audit(r.Context(), "channel.test", ch.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type alertRuleResponse struct {
	ID                    string          `json:"id"`
	Name                  string          `json:"name"`
	Type                  string          `json:"type"`
	Threshold             int32           `json:"threshold"`
	WindowMinutes         int32           `json:"window_minutes"`
	MetricName            string          `json:"metric_name,omitempty"`
	MetricLabels          json.RawMessage `json:"metric_labels,omitempty"`
	BusinessCheckName     string          `json:"business_check_name,omitempty"`
	Comparison            string          `json:"comparison,omitempty"`
	ThresholdValue        float64         `json:"threshold_value"`
	SpanName              string          `json:"span_name,omitempty"`
	Percentile            int32           `json:"percentile,omitempty"`
	ConnectorProvider     string          `json:"connector_provider,omitempty"`
	ConnectorOperation    string          `json:"connector_operation,omitempty"`
	ConnectorChannel      string          `json:"connector_channel,omitempty"`
	ConnectorErrorCode    string          `json:"connector_error_code,omitempty"`
	ConnectorConnectionID string          `json:"connector_connection_id,omitempty"`
	ConnectorSourceKind   string          `json:"connector_source_kind,omitempty"`
	ConnectorSyncMode     string          `json:"connector_sync_mode,omitempty"`
	ConnectorQuotaMetric  string          `json:"connector_quota_metric,omitempty"`
	ReleaseVersion        string          `json:"release_version,omitempty"`
	ServiceName           string          `json:"service_name,omitempty"`
	LogSeverity           string          `json:"log_severity,omitempty"`
	UsageSurface          string          `json:"usage_surface,omitempty"`
	UsageOperation        string          `json:"usage_operation,omitempty"`
	UsageMeasure          string          `json:"usage_measure,omitempty"`
	APIKeyHygieneMode     string          `json:"api_key_hygiene_mode,omitempty"`
	AITriageMode          string          `json:"ai_triage_mode,omitempty"`
	Enabled               bool            `json:"enabled"`
}

func toAlertRuleResponse(rule *generated.AlertRule) alertRuleResponse {
	if rule == nil {
		return alertRuleResponse{}
	}
	out := alertRuleResponse{
		ID: rule.ID, Name: rule.Name, Type: rule.Type, Threshold: rule.Threshold, WindowMinutes: rule.WindowMinutes,
		MetricName: rule.MetricName, MetricLabels: rule.MetricLabels, Comparison: rule.Comparison, ThresholdValue: rule.ThresholdValue,
		SpanName: rule.SpanName, Percentile: rule.Percentile,
		ConnectorProvider: rule.ConnectorProvider, ConnectorOperation: rule.ConnectorOperation, ConnectorChannel: rule.ConnectorChannel, ConnectorErrorCode: rule.ConnectorErrorCode, ConnectorConnectionID: rule.ConnectorConnectionID, ConnectorSourceKind: rule.ConnectorSourceKind, ConnectorSyncMode: rule.ConnectorSyncMode, ConnectorQuotaMetric: rule.ConnectorQuotaMetric,
		ReleaseVersion: rule.ReleaseVersion, ServiceName: rule.ServiceName, LogSeverity: rule.LogSeverity, APIKeyHygieneMode: rule.ApiKeyHygieneMode, AITriageMode: rule.AiTriageMode, BusinessCheckName: rule.BusinessCheckName, Enabled: rule.Enabled,
	}
	if rule.Type == "usage_threshold" {
		out.UsageSurface, out.UsageOperation, out.UsageMeasure = rule.UsageSurface, rule.UsageOperation, rule.UsageMeasure
	}
	return out
}

func validMetricComparison(comparison string) bool {
	switch comparison {
	case "gt", "gte", "lt", "lte":
		return true
	default:
		return false
	}
}

// normalizeMetricThreshold validates and canonicalizes the user-controlled
// fields of a metric threshold rule before they reach storage. Keeping this
// pure makes the bounds easy to test without a live database or authenticated
// HTTP request.
func normalizeMetricThreshold(metricName, comparison string, threshold *float64, windowMinutes int32) (string, string, float64, error) {
	metricName = strings.TrimSpace(metricName)
	if metricName == "" || len(metricName) > 128 || strings.ContainsRune(metricName, '\x00') {
		return "", "", 0, errors.New("metric_name is required and must be at most 128 bytes")
	}
	comparison = strings.ToLower(strings.TrimSpace(comparison))
	if comparison == "" {
		comparison = "gte"
	}
	if !validMetricComparison(comparison) {
		return "", "", 0, errors.New("comparison must be gt, gte, lt or lte")
	}
	if windowMinutes < 1 || windowMinutes > 720 {
		return "", "", 0, errors.New("metric threshold windows must be between 1 and 720 minutes")
	}
	if threshold == nil || math.IsNaN(*threshold) || math.IsInf(*threshold, 0) {
		return "", "", 0, errors.New("threshold_value must be a finite number")
	}
	return metricName, comparison, *threshold, nil
}

const (
	maxMetricSelectorLabels = 5
	maxMetricSelectorValue  = 128
)

// normalizeMetricLabels keeps metric alert selectors aligned with the labels
// emitted by Flare's browser and mobile bridges. Arbitrary JSON selectors would
// turn a rule into an unbounded query surface, so only the low-cardinality
// dimensions with a documented meaning are accepted.
func normalizeMetricLabels(raw map[string]string) (json.RawMessage, error) {
	if len(raw) > maxMetricSelectorLabels {
		return nil, errors.New("metric_labels may contain at most 5 selectors")
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > maxMetricSelectorValue || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("metric_labels.%s must be a non-empty value of at most 128 bytes", key)
		}
		switch key {
		case "environment", "release":
			if !safeMetricSelectorToken(value) {
				return nil, fmt.Errorf("metric_labels.%s contains unsupported characters", key)
			}
		case "method":
			if !isUpperToken(value, 12) {
				return nil, errors.New("metric_labels.method must contain only uppercase letters")
			}
		case "status_class":
			if len(value) != 3 || value[1:] != "xx" || value[0] < '1' || value[0] > '5' {
				return nil, errors.New("metric_labels.status_class must be an HTTP status class")
			}
		case "route":
			if !safeMetricSelectorToken(value) {
				return nil, errors.New("metric_labels.route contains unsupported characters")
			}
		default:
			return nil, fmt.Errorf("metric_labels.%s is not an allowed selector", key)
		}
		out[key] = value
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, errors.New("metric_labels could not be encoded")
	}
	if len(encoded) > 1024 {
		return nil, errors.New("metric_labels is too large")
	}
	return json.RawMessage(encoded), nil
}

func safeMetricSelectorToken(value string) bool {
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._:@/-", r) {
			continue
		}
		return false
	}
	return value != ""
}

func isUpperToken(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, r := range value {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// normalizeTracePercentile validates one span-operation percentile rule. The
// percentile set is intentionally small and explicit: these are the common
// SLO views, and bounding it prevents accidental high-cardinality rule
// configurations while keeping the watchdog query predictable.
func normalizeTracePercentile(spanName, comparison string, percentile int32, threshold *float64, windowMinutes int32) (string, string, int32, float64, error) {
	spanName = strings.TrimSpace(spanName)
	if spanName == "" || len(spanName) > 128 || strings.ContainsRune(spanName, '\x00') {
		return "", "", 0, 0, errors.New("span_name is required and must be at most 128 bytes")
	}
	comparison = strings.ToLower(strings.TrimSpace(comparison))
	if comparison == "" {
		comparison = "gte"
	}
	if !validMetricComparison(comparison) {
		return "", "", 0, 0, errors.New("comparison must be gt, gte, lt or lte")
	}
	if percentile == 0 {
		percentile = 95
	}
	switch percentile {
	case 50, 75, 90, 95, 99:
	default:
		return "", "", 0, 0, errors.New("percentile must be one of 50, 75, 90, 95 or 99")
	}
	if windowMinutes < 1 || windowMinutes > 720 {
		return "", "", 0, 0, errors.New("trace percentile windows must be between 1 and 720 minutes")
	}
	if threshold == nil || math.IsNaN(*threshold) || math.IsInf(*threshold, 0) || *threshold < 0 || *threshold > 24*60*60*1000 {
		return "", "", 0, 0, errors.New("threshold_value must be a finite latency in milliseconds between 0 and 86400000")
	}
	return spanName, comparison, percentile, *threshold, nil
}

// normalizeServiceLevel validates one availability/error-budget rule over an
// exact span operation.  Service-level rules reuse the existing bounded alert
// columns: threshold is the latency objective in milliseconds and
// threshold_value is the minimum target availability (for example 0.999).
// Keeping the target decimal separate from the latency objective avoids
// silently treating a percentile latency threshold as an availability SLO.
func normalizeServiceLevel(spanName string, target *float64, latencyMS, windowMinutes int32) (string, float64, int32, int32, error) {
	spanName = strings.TrimSpace(spanName)
	if spanName == "" || len(spanName) > 128 || strings.ContainsRune(spanName, '\x00') {
		return "", 0, 0, 0, errors.New("span_name is required and must be at most 128 bytes")
	}
	if windowMinutes < 1 || windowMinutes > 720 {
		return "", 0, 0, 0, errors.New("service-level windows must be between 1 and 720 minutes")
	}
	if latencyMS == 0 {
		latencyMS = 1000
	}
	if latencyMS < 1 || latencyMS > 24*60*60*1000 {
		return "", 0, 0, 0, errors.New("threshold must be a latency objective between 1 and 86400000 milliseconds")
	}
	if target == nil {
		defaultTarget := 0.999
		target = &defaultTarget
	}
	if math.IsNaN(*target) || math.IsInf(*target, 0) || *target <= 0 || *target >= 1 {
		return "", 0, 0, 0, errors.New("threshold_value must be a finite availability target greater than 0 and less than 1")
	}
	return spanName, *target, latencyMS, windowMinutes, nil
}

// normalizeConnectorErrorRate bounds a project-scoped connector selector and
// keeps the error-rate rule meaningful. Provider is required; empty operation
// and channel selectors intentionally match every operation/channel for that
// provider. Selectors are labels only and never carry URLs or response text.
func normalizeConnectorSourceKind(raw string) (string, error) {
	source := strings.ToLower(strings.TrimSpace(raw))
	if source == "" {
		return "server_observed", nil
	}
	if source != "server_observed" && source != "client_reported" {
		return "", errors.New("connector_source_kind must be server_observed or client_reported")
	}
	return source, nil
}

func normalizeConnectorErrorRate(provider, operation, channel, errorCode, comparison string, threshold *float64, windowMinutes int32) (string, string, string, string, string, float64, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	operation = strings.ToLower(strings.TrimSpace(operation))
	channel = strings.ToLower(strings.TrimSpace(channel))
	errorCode = strings.ToLower(strings.TrimSpace(errorCode))
	if provider == "" || len(provider) > 32 || strings.ContainsRune(provider, '\x00') {
		return "", "", "", "", "", 0, errors.New("connector_provider is required and must be at most 32 bytes")
	}
	if len(operation) > 64 || strings.ContainsRune(operation, '\x00') {
		return "", "", "", "", "", 0, errors.New("connector_operation must be at most 64 bytes")
	}
	if len(channel) > 32 || strings.ContainsRune(channel, '\x00') {
		return "", "", "", "", "", 0, errors.New("connector_channel must be at most 32 bytes")
	}
	if errorCode != "" && !connectorErrorCodeToken.MatchString(errorCode) {
		return "", "", "", "", "", 0, errors.New("connector_error_code must be a safe bounded token")
	}
	comparison = strings.ToLower(strings.TrimSpace(comparison))
	if comparison == "" {
		comparison = "gte"
	}
	if !validMetricComparison(comparison) {
		return "", "", "", "", "", 0, errors.New("comparison must be gt, gte, lt or lte")
	}
	if windowMinutes < 1 || windowMinutes > 720 {
		return "", "", "", "", "", 0, errors.New("connector error-rate windows must be between 1 and 720 minutes")
	}
	if threshold == nil || math.IsNaN(*threshold) || math.IsInf(*threshold, 0) || *threshold < 0 || *threshold > 1 {
		return "", "", "", "", "", 0, errors.New("threshold_value must be a finite number between 0 and 1")
	}
	return provider, operation, channel, errorCode, comparison, *threshold, nil
}

// normalizeBusinessHealth validates one exact, caller-defined business check
// series. The check name is only a bounded series key; all observed values are
// fixed outcome/latency/status fields, so the watchdog never executes caller
// SQL or evaluates an arbitrary selector.
func normalizeBusinessHealth(checkName, comparison string, threshold *float64, windowMinutes int32) (string, string, float64, error) {
	checkName = strings.ToLower(strings.TrimSpace(checkName))
	if !businessCheckNameToken.MatchString(checkName) {
		return "", "", 0, errors.New("business_check_name is required and must be a bounded lowercase token")
	}
	comparison = strings.ToLower(strings.TrimSpace(comparison))
	if comparison == "" {
		comparison = "gte"
	}
	if comparison != "gt" && comparison != "gte" {
		return "", "", 0, errors.New("business health comparison must be gt or gte")
	}
	if windowMinutes < 1 || windowMinutes > 720 {
		return "", "", 0, errors.New("business health windows must be between 1 and 720 minutes")
	}
	if threshold == nil || math.IsNaN(*threshold) || math.IsInf(*threshold, 0) || *threshold < 0 || *threshold > 1 {
		return "", "", 0, errors.New("threshold_value must be a finite error rate between 0 and 1")
	}
	return checkName, comparison, *threshold, nil
}

// normalizeConnectorQuota validates a latest-observation rule for one bounded
// connector series. The metric is explicit because quota_remaining,
// retry_after_ms, and sync_lag_ms have different operational meanings and
// units. Empty
// operation/channel selectors match all values for the provider.
func normalizeConnectorQuota(provider, operation, channel, metric, comparison string, threshold *float64, windowMinutes int32) (string, string, string, string, string, float64, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	operation = strings.ToLower(strings.TrimSpace(operation))
	channel = strings.ToLower(strings.TrimSpace(channel))
	if provider == "" || len(provider) > 32 || strings.ContainsRune(provider, '\x00') {
		return "", "", "", "", "", 0, errors.New("connector_provider is required and must be at most 32 bytes")
	}
	if len(operation) > 64 || strings.ContainsRune(operation, '\x00') {
		return "", "", "", "", "", 0, errors.New("connector_operation must be at most 64 bytes")
	}
	if len(channel) > 32 || strings.ContainsRune(channel, '\x00') {
		return "", "", "", "", "", 0, errors.New("connector_channel must be at most 32 bytes")
	}
	metric = strings.ToLower(strings.TrimSpace(metric))
	if metric == "" {
		metric = "quota_remaining"
	}
	if metric != "quota_remaining" && metric != "retry_after_ms" && metric != "sync_lag_ms" {
		return "", "", "", "", "", 0, errors.New("connector_quota_metric must be quota_remaining, retry_after_ms or sync_lag_ms")
	}
	comparison = strings.ToLower(strings.TrimSpace(comparison))
	if comparison == "" {
		comparison = "lte"
	}
	if !validMetricComparison(comparison) {
		return "", "", "", "", "", 0, errors.New("comparison must be gt, gte, lt or lte")
	}
	if windowMinutes < 1 || windowMinutes > 720 {
		return "", "", "", "", "", 0, errors.New("connector quota windows must be between 1 and 720 minutes")
	}
	if threshold == nil || math.IsNaN(*threshold) || math.IsInf(*threshold, 0) || *threshold < 0 {
		return "", "", "", "", "", 0, errors.New("threshold_value must be a finite non-negative number")
	}
	max := float64(maxConnectorQuotaRemaining)
	if metric == "retry_after_ms" {
		max = float64(maxConnectorRetryAfterMS)
	} else if metric == "sync_lag_ms" {
		max = float64(maxConnectorSyncLagMS)
	}
	if *threshold > max {
		return "", "", "", "", "", 0, errors.New("threshold_value is outside the selected connector quota metric range")
	}
	return provider, operation, channel, metric, comparison, *threshold, nil
}

// normalizeConnectorFreshness validates the bounded connector series whose
// completed-at cadence is monitored. Empty operation and channel selectors
// intentionally match every series for the provider; the watchdog still
// scopes every read to the owning project and organization.
func normalizeConnectorFreshness(provider, operation, channel string, windowMinutes int32) (string, string, string, int32, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	operation = strings.ToLower(strings.TrimSpace(operation))
	channel = strings.ToLower(strings.TrimSpace(channel))
	if provider == "" || len(provider) > 32 || strings.ContainsRune(provider, '\x00') {
		return "", "", "", 0, errors.New("connector_provider is required and must be at most 32 bytes")
	}
	if len(operation) > 64 || strings.ContainsRune(operation, '\x00') {
		return "", "", "", 0, errors.New("connector_operation must be at most 64 bytes")
	}
	if len(channel) > 32 || strings.ContainsRune(channel, '\x00') {
		return "", "", "", 0, errors.New("connector_channel must be at most 32 bytes")
	}
	if windowMinutes < 1 || windowMinutes > 720 {
		return "", "", "", 0, errors.New("connector freshness windows must be between 1 and 720 minutes")
	}
	return provider, operation, channel, windowMinutes, nil
}

// normalizeLogSeverity validates one canonical log severity selector. The
// aliases keep SDKs that use the common "warning" and "critical" names
// interoperable while storage and watchdog queries stay on one bounded set.
func normalizeLogSeverity(severity string, threshold, windowMinutes int32) (string, int32, int32, error) {
	severity = strings.ToLower(strings.TrimSpace(severity))
	switch severity {
	case "warning":
		severity = "warn"
	case "critical":
		severity = "fatal"
	}
	switch severity {
	case "trace", "debug", "info", "warn", "error", "fatal":
	default:
		return "", 0, 0, errors.New("log_severity must be one of trace, debug, info, warn, error or fatal")
	}
	if len(severity) > 8 || strings.ContainsRune(severity, '\x00') {
		return "", 0, 0, errors.New("log_severity must be at most 8 bytes")
	}
	if windowMinutes < 1 || windowMinutes > 720 {
		return "", 0, 0, errors.New("log severity windows must be between 1 and 720 minutes")
	}
	if threshold < 1 || threshold > 1_000_000 {
		return "", 0, 0, errors.New("log severity threshold must be between 1 and 1000000 records")
	}
	return severity, threshold, windowMinutes, nil
}

// normalizeReleaseErrorRate validates the optional release selector. An empty
// selector means "latest recent release"; a non-empty selector is resolved by
// the tenant/project-scoped watchdog query and is never treated as a global
// release name.
func normalizeReleaseErrorRate(version, comparison string, threshold *float64, windowMinutes int32) (string, string, float64, error) {
	version = strings.TrimSpace(version)
	if len(version) > 128 || strings.ContainsRune(version, '\x00') {
		return "", "", 0, errors.New("release_version must be at most 128 bytes")
	}
	comparison = strings.ToLower(strings.TrimSpace(comparison))
	if comparison == "" {
		comparison = "gte"
	}
	if !validMetricComparison(comparison) {
		return "", "", 0, errors.New("comparison must be gt, gte, lt or lte")
	}
	if windowMinutes < 1 || windowMinutes > 720 {
		return "", "", 0, errors.New("release error-rate windows must be between 1 and 720 minutes")
	}
	if threshold == nil || math.IsNaN(*threshold) || math.IsInf(*threshold, 0) || *threshold < 0 || *threshold > 1 {
		return "", "", 0, errors.New("threshold_value must be a finite number between 0 and 1")
	}
	return version, comparison, *threshold, nil
}

// normalizeReleaseFreshness requires an explicit release selector. A
// freshness rule without one would silently follow whichever release happens
// to be newest, and a release row may have been created by old event traffic
// rather than a deploy. The deploy marker and heartbeat checks happen in the
// watchdog query, but the selector is required at rule creation time.
func normalizeReleaseFreshness(version string, windowMinutes int32) (string, int32, error) {
	version = strings.TrimSpace(version)
	if version == "" || len(version) > 128 || strings.ContainsRune(version, '\x00') {
		return "", 0, errors.New("release_version is required and must be at most 128 bytes")
	}
	if windowMinutes < 1 || windowMinutes > 720 {
		return "", 0, errors.New("release freshness windows must be between 1 and 720 minutes")
	}
	return version, windowMinutes, nil
}

// normalizeServiceFreshness validates one exact canonical service-up
// heartbeat dimension. The service label is kept case-sensitive because the
// heartbeat message is an exact string; control characters and oversized
// values are rejected before they reach the watchdog query.
func normalizeServiceFreshness(service, release string, windowMinutes int32) (string, string, int32, error) {
	service = strings.TrimSpace(service)
	release = strings.TrimSpace(release)
	if service == "" || len(service) > 64 || strings.ContainsRune(service, '\x00') || strings.IndexFunc(service, unicode.IsControl) >= 0 {
		return "", "", 0, errors.New("service_name is required and must be at most 64 bytes")
	}
	if release == "" || len(release) > 128 || strings.ContainsRune(release, '\x00') {
		return "", "", 0, errors.New("release_version is required and must be at most 128 bytes")
	}
	if windowMinutes < 1 || windowMinutes > 720 {
		return "", "", 0, errors.New("service freshness windows must be between 1 and 720 minutes")
	}
	return service, release, windowMinutes, nil
}

var usageThresholdOperations = map[string]map[string]struct{}{
	"dsn": {
		"envelope": {}, "event": {}, "logs": {}, "metrics": {}, "traces": {},
		"connector_attempts": {}, "checkins": {}, "symbols": {},
		"otlp_logs": {}, "otlp_traces": {}, "otlp_metrics": {},
	},
	"api_key":      {"read": {}, "write": {}, "mcp": {}},
	"business_key": {"business_checks": {}},
	"mcp": {
		"initialize": {}, "ping": {}, "tools_list": {}, "tools_call": {},
		"tool_overview": {}, "tool_list_projects": {}, "tool_list_issues": {},
		"tool_get_issue": {}, "tool_search_logs": {}, "tool_get_trace": {},
		"tool_query_metrics": {}, "tool_update_issue_status": {}, "tool_triage_issue": {},
	},
}

func normalizeUsageThreshold(surface, operation, measure, comparison string, threshold, windowMinutes int32) (string, string, string, string, int32, int32, error) {
	surface = strings.ToLower(strings.TrimSpace(surface))
	operations, ok := usageThresholdOperations[surface]
	if !ok {
		return "", "", "", "", 0, 0, errors.New("usage_surface must be dsn, api_key, mcp or business_key")
	}
	operation = strings.ToLower(strings.TrimSpace(operation))
	if _, ok := operations[operation]; !ok {
		return "", "", "", "", 0, 0, errors.New("usage_operation is not supported for this usage surface")
	}
	measure = strings.ToLower(strings.TrimSpace(measure))
	if measure == "" {
		measure = "requests"
	}
	switch measure {
	case "requests", "errors", "denied":
	default:
		return "", "", "", "", 0, 0, errors.New("usage_measure must be requests, errors or denied")
	}
	comparison = strings.ToLower(strings.TrimSpace(comparison))
	if comparison == "" {
		comparison = "gte"
	}
	if !validMetricComparison(comparison) {
		return "", "", "", "", 0, 0, errors.New("comparison must be gt, gte, lt or lte")
	}
	if threshold < 1 || threshold > 1_000_000_000 {
		return "", "", "", "", 0, 0, errors.New("usage threshold must be between 1 and 1000000000")
	}
	if windowMinutes < 1 || windowMinutes > 720 {
		return "", "", "", "", 0, 0, errors.New("usage threshold windows must be between 1 and 720 minutes")
	}
	return surface, operation, measure, comparison, threshold, windowMinutes, nil
}

const maxAPIKeyHygieneWindowMinutes = 90 * 24 * 60

// normalizeAPIKeyHygiene validates the fixed credential-health modes. The
// horizon is a key-age/expiry horizon in minutes, rather than an event
// window: unused counts credentials older than it, while expiring counts
// credentials whose expiry is within it. Keeping one bounded integer makes the
// watchdog query predictable and avoids arbitrary key metadata selectors.
func normalizeAPIKeyHygiene(mode string, threshold, windowMinutes int32) (string, int32, int32, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "unused", "expiring":
	default:
		return "", 0, 0, errors.New("api_key_hygiene_mode must be unused or expiring")
	}
	if windowMinutes < 1 || windowMinutes > maxAPIKeyHygieneWindowMinutes {
		return "", 0, 0, fmt.Errorf("api key hygiene horizons must be between 1 and %d minutes", maxAPIKeyHygieneWindowMinutes)
	}
	if threshold < 1 || threshold > 1_000_000 {
		return "", 0, 0, errors.New("api key hygiene thresholds must be between 1 and 1000000")
	}
	return mode, threshold, windowMinutes, nil
}

const maxAITriageWindowMinutes = 720

// normalizeAITriage validates the fixed AI receipt health modes. A failed rule
// counts terminal failed triage receipts in the rolling window; a stuck rule
// counts started receipts that have exceeded the same bounded age horizon.
// Keeping the selector fixed prevents the public alert API from becoming an
// arbitrary receipt query.
func normalizeAITriage(mode string, threshold, windowMinutes int32) (string, int32, int32, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "failed", "stuck":
	default:
		return "", 0, 0, errors.New("ai_triage_mode must be failed or stuck")
	}
	if windowMinutes < 1 || windowMinutes > maxAITriageWindowMinutes {
		return "", 0, 0, errors.New("AI triage windows must be between 1 and 720 minutes")
	}
	if threshold < 1 || threshold > 1_000_000 {
		return "", 0, 0, errors.New("AI triage thresholds must be between 1 and 1000000")
	}
	return mode, threshold, windowMinutes, nil
}

const maxIngestAcceptanceWindowMinutes = 720

// normalizeIngestAcceptance validates the fixed request-receipt health rule.
// The threshold is the number of incomplete receipts and the window is the
// maximum age they may remain in_progress. No caller-controlled SQL selector
// is needed, keeping the watchdog aggregate bounded to one project/org.
func normalizeIngestAcceptance(threshold, windowMinutes int32) (int32, int32, error) {
	if windowMinutes < 1 || windowMinutes > maxIngestAcceptanceWindowMinutes {
		return 0, 0, errors.New("ingest acceptance windows must be between 1 and 720 minutes")
	}
	if threshold < 1 || threshold > 1_000_000 {
		return 0, 0, errors.New("ingest acceptance thresholds must be between 1 and 1000000")
	}
	return threshold, windowMinutes, nil
}

// normalizeConnectorSyncHealth validates one durable sync-state health rule.
// Sync state belongs to the organization-owned connector registry, so an
// exact registered connection and operation are required. `failed` counts
// recent terminal failures; `overdue` uses the same bounded window as the
// grace period for a missed schedule or expired lease.
func normalizeConnectorSyncHealth(provider, operation, mode string, threshold, windowMinutes int32) (string, string, string, int32, int32, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	operation = strings.ToLower(strings.TrimSpace(operation))
	mode = strings.ToLower(strings.TrimSpace(mode))
	if provider == "" || len(provider) > 32 || strings.ContainsRune(provider, '\x00') {
		return "", "", "", 0, 0, errors.New("connector_provider is required and must be at most 32 bytes")
	}
	if operation == "" || len(operation) > 64 || strings.ContainsRune(operation, '\x00') {
		return "", "", "", 0, 0, errors.New("connector_operation is required and must be at most 64 bytes")
	}
	if mode == "" {
		mode = "overdue"
	}
	if mode != "failed" && mode != "overdue" {
		return "", "", "", 0, 0, errors.New("connector_sync_mode must be failed or overdue")
	}
	if threshold < 1 || threshold > 1_000_000 {
		return "", "", "", 0, 0, errors.New("connector sync health thresholds must be between 1 and 1000000")
	}
	if windowMinutes < 1 || windowMinutes > 720 {
		return "", "", "", 0, 0, errors.New("connector sync health windows must be between 1 and 720 minutes")
	}
	return provider, operation, mode, threshold, windowMinutes, nil
}

func (s *Server) handleCreateAlertRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name                  string            `json:"name"`
		Type                  string            `json:"type"`
		Threshold             int32             `json:"threshold"`
		WindowMinutes         int32             `json:"window_minutes"`
		MetricName            string            `json:"metric_name"`
		MetricLabels          map[string]string `json:"metric_labels"`
		BusinessCheckName     string            `json:"business_check_name"`
		SpanName              string            `json:"span_name"`
		Percentile            int32             `json:"percentile"`
		Comparison            string            `json:"comparison"`
		ConnectorProvider     string            `json:"connector_provider"`
		ConnectorOperation    string            `json:"connector_operation"`
		ConnectorChannel      string            `json:"connector_channel"`
		ConnectorErrorCode    string            `json:"connector_error_code"`
		ConnectorConnectionID string            `json:"connector_connection_id"`
		ConnectorSourceKind   string            `json:"connector_source_kind"`
		ConnectorSyncMode     string            `json:"connector_sync_mode"`
		ConnectorQuotaMetric  string            `json:"connector_quota_metric"`
		ReleaseVersion        string            `json:"release_version"`
		ServiceName           string            `json:"service_name"`
		LogSeverity           string            `json:"log_severity"`
		UsageSurface          string            `json:"usage_surface"`
		UsageOperation        string            `json:"usage_operation"`
		UsageMeasure          string            `json:"usage_measure"`
		APIKeyHygieneMode     string            `json:"api_key_hygiene_mode"`
		AITriageMode          string            `json:"ai_triage_mode"`
		// Pointer distinguishes an omitted decimal threshold from an explicit 0.
		ThresholdValue *float64 `json:"threshold_value"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Type == "" {
		req.Type = "new_issue"
	}
	metricName := ""
	businessCheckName := ""
	metricLabels := json.RawMessage(`{}`)
	spanName := ""
	percentile := int32(95)
	comparison := "gte"
	thresholdValue := float64(0)
	connectorProvider, connectorOperation, connectorChannel, connectorErrorCode, connectorConnectionID, connectorSyncMode := "", "", "", "", "", ""
	connectorSourceKind := ""
	connectorQuotaMetric := ""
	releaseVersion := ""
	serviceName := ""
	logSeverity := ""
	// The database requires usage_measure even for unrelated alert types.
	// Keep its schema default instead of sending an empty value on INSERT.
	usageSurface, usageOperation, usageMeasure := "", "", "requests"
	apiKeyHygieneMode := ""
	aiTriageMode := ""
	var err error
	switch req.Type {
	case "new_issue", "regression":
		req.Threshold, req.WindowMinutes = 0, 0
	case "spike":
		if req.Threshold < 1 || req.WindowMinutes < 1 {
			writeErr(w, http.StatusBadRequest, "spike rules need a threshold (events) and window (minutes) of at least 1")
			return
		}
	case "anomaly":
		// threshold = baseline multiplier (>=2), window_minutes = the recent
		// window compared against the trailing 24h baseline. Cap the window at
		// 720m (12h): beyond that there is <2 windows in a day, so there is no
		// baseline to compare against and the rule could never fire.
		if req.WindowMinutes < 1 || req.WindowMinutes > 720 {
			writeErr(w, http.StatusBadRequest, "anomaly rules need a window (minutes) between 1 and 720")
			return
		}
		if req.Threshold < 2 {
			req.Threshold = 3 // default: fire at 3x baseline
		}
	case "silence":
		// threshold = minimum events over 24h to count as "normally active",
		// window_minutes = how long silent before firing.
		if req.WindowMinutes < 1 {
			writeErr(w, http.StatusBadRequest, "silence rules need a window (minutes) of at least 1")
			return
		}
		if req.Threshold < 1 {
			req.Threshold = 20
		}
	case "metric_threshold":
		var validationErr error
		metricName, comparison, thresholdValue, validationErr = normalizeMetricThreshold(
			req.MetricName, req.Comparison, req.ThresholdValue, req.WindowMinutes,
		)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
		metricLabels, validationErr = normalizeMetricLabels(req.MetricLabels)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "trace_percentile":
		var validationErr error
		spanName, comparison, percentile, thresholdValue, validationErr = normalizeTracePercentile(
			req.SpanName, req.Comparison, req.Percentile, req.ThresholdValue, req.WindowMinutes,
		)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "service_level":
		var validationErr error
		spanName, thresholdValue, req.Threshold, req.WindowMinutes, validationErr = normalizeServiceLevel(
			req.SpanName, req.ThresholdValue, req.Threshold, req.WindowMinutes,
		)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
		// The watchdog compares observed availability directly to the target;
		// keep the stored comparison explicit for API consumers and old clients.
		comparison = "lt"
	case "business_health":
		var validationErr error
		businessCheckName, comparison, thresholdValue, validationErr = normalizeBusinessHealth(
			req.BusinessCheckName, req.Comparison, req.ThresholdValue, req.WindowMinutes,
		)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "connector_error_rate":
		connectorSourceKind, err = normalizeConnectorSourceKind(req.ConnectorSourceKind)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		var validationErr error
		connectorProvider, connectorOperation, connectorChannel, connectorErrorCode, comparison, thresholdValue, validationErr = normalizeConnectorErrorRate(
			req.ConnectorProvider, req.ConnectorOperation, req.ConnectorChannel, req.ConnectorErrorCode, req.Comparison, req.ThresholdValue, req.WindowMinutes,
		)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "connector_quota":
		connectorSourceKind, err = normalizeConnectorSourceKind(req.ConnectorSourceKind)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		var validationErr error
		connectorProvider, connectorOperation, connectorChannel, connectorQuotaMetric, comparison, thresholdValue, validationErr = normalizeConnectorQuota(
			req.ConnectorProvider, req.ConnectorOperation, req.ConnectorChannel, req.ConnectorQuotaMetric, req.Comparison, req.ThresholdValue, req.WindowMinutes,
		)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "connector_freshness":
		connectorSourceKind, err = normalizeConnectorSourceKind(req.ConnectorSourceKind)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		var validationErr error
		connectorProvider, connectorOperation, connectorChannel, req.WindowMinutes, validationErr = normalizeConnectorFreshness(
			req.ConnectorProvider, req.ConnectorOperation, req.ConnectorChannel, req.WindowMinutes,
		)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "connector_sync_health":
		var validationErr error
		connectorProvider, connectorOperation, connectorSyncMode, req.Threshold, req.WindowMinutes, validationErr = normalizeConnectorSyncHealth(
			req.ConnectorProvider, req.ConnectorOperation, req.ConnectorSyncMode, req.Threshold, req.WindowMinutes,
		)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "log_severity":
		var validationErr error
		logSeverity, req.Threshold, req.WindowMinutes, validationErr = normalizeLogSeverity(
			req.LogSeverity, req.Threshold, req.WindowMinutes,
		)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "release_error_rate":
		var validationErr error
		releaseVersion, comparison, thresholdValue, validationErr = normalizeReleaseErrorRate(
			req.ReleaseVersion, req.Comparison, req.ThresholdValue, req.WindowMinutes,
		)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "release_freshness":
		var validationErr error
		releaseVersion, req.WindowMinutes, validationErr = normalizeReleaseFreshness(req.ReleaseVersion, req.WindowMinutes)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "service_freshness":
		var validationErr error
		serviceName, releaseVersion, req.WindowMinutes, validationErr = normalizeServiceFreshness(req.ServiceName, req.ReleaseVersion, req.WindowMinutes)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "usage_threshold":
		var validationErr error
		usageSurface, usageOperation, usageMeasure, comparison, req.Threshold, req.WindowMinutes, validationErr = normalizeUsageThreshold(
			req.UsageSurface, req.UsageOperation, req.UsageMeasure, req.Comparison, req.Threshold, req.WindowMinutes,
		)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "deploy_failure":
		if req.WindowMinutes < 1 || req.WindowMinutes > 720 {
			writeErr(w, http.StatusBadRequest, "deploy failure rules need a window (minutes) between 1 and 720")
			return
		}
		if req.Threshold < 1 {
			req.Threshold = 1
		}
		if req.Threshold > 1_000_000 {
			writeErr(w, http.StatusBadRequest, "deploy failure thresholds must be between 1 and 1000000")
			return
		}
	case "deploy_acceptance":
		if req.WindowMinutes < 1 || req.WindowMinutes > 720 {
			writeErr(w, http.StatusBadRequest, "deploy acceptance rules need a grace window (minutes) between 1 and 720")
			return
		}
		if req.Threshold < 1 {
			req.Threshold = 1
		}
		if req.Threshold > 1_000_000 {
			writeErr(w, http.StatusBadRequest, "deploy acceptance thresholds must be between 1 and 1000000")
			return
		}
	case "rollback":
		if req.WindowMinutes < 1 || req.WindowMinutes > 720 {
			writeErr(w, http.StatusBadRequest, "rollback rules need a window (minutes) between 1 and 720")
			return
		}
		if req.Threshold < 1 {
			req.Threshold = 1
		}
		if req.Threshold > 1_000_000 {
			writeErr(w, http.StatusBadRequest, "rollback thresholds must be between 1 and 1000000")
			return
		}
	case "api_key_hygiene":
		var validationErr error
		apiKeyHygieneMode, req.Threshold, req.WindowMinutes, validationErr = normalizeAPIKeyHygiene(
			req.APIKeyHygieneMode, req.Threshold, req.WindowMinutes,
		)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "ai_triage":
		var validationErr error
		aiTriageMode, req.Threshold, req.WindowMinutes, validationErr = normalizeAITriage(
			req.AITriageMode, req.Threshold, req.WindowMinutes,
		)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	case "ingest_acceptance":
		var validationErr error
		req.Threshold, req.WindowMinutes, validationErr = normalizeIngestAcceptance(req.Threshold, req.WindowMinutes)
		if validationErr != nil {
			writeErr(w, http.StatusBadRequest, validationErr.Error())
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "type must be new_issue, regression, spike, anomaly, silence, metric_threshold, trace_percentile, service_level, business_health, connector_error_rate, connector_quota, connector_freshness, connector_sync_health, service_freshness, release_error_rate, release_freshness, deploy_failure, deploy_acceptance, rollback, log_severity, usage_threshold, api_key_hygiene, ai_triage or ingest_acceptance")
		return
	}
	if strings.TrimSpace(req.ConnectorSourceKind) != "" && connectorSourceKind == "" {
		writeErr(w, http.StatusBadRequest, "connector_source_kind is only valid for connector error, quota or freshness rules")
		return
	}

	// Verify the path project belongs to the caller's org before writing a rule
	// against it. projects.id is a global primary key, so without this a member
	// could persist an alert rule referencing another org's project. Mirrors the
	// ownership check every project read path performs.
	proj, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{
		ID:       chi.URLParam(r, "id"),
		OrgScope: orgIDFrom(r.Context()),
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	// A connection selector is meaningful only for connector rules. Resolve it
	// through the caller's organization before persisting the rule, and require
	// the selected installation's provider to match the normalized provider.
	// This keeps a guessed ID from becoming a cross-tenant selector and prevents
	// a rule from silently watching a different provider than its label says.
	if strings.TrimSpace(req.ConnectorConnectionID) != "" {
		if connectorSourceKind == "client_reported" {
			writeErr(w, http.StatusBadRequest, "client_reported connector rules cannot select a registered connection")
			return
		}
		if req.Type != "connector_error_rate" && req.Type != "connector_quota" && req.Type != "connector_freshness" && req.Type != "connector_sync_health" {
			writeErr(w, http.StatusBadRequest, "connector_connection_id is only valid for connector rules")
			return
		}
		connectorConnectionID = strings.TrimSpace(req.ConnectorConnectionID)
		if len([]byte(connectorConnectionID)) > 128 || !connectorProviderToken.MatchString(connectorConnectionID) {
			writeErr(w, http.StatusBadRequest, "connector_connection_id must be a safe bounded identifier")
			return
		}
		connection, connectionErr := s.q.GetConnectorConnectionByID(r.Context(), generated.GetConnectorConnectionByIDParams{
			ID: connectorConnectionID, OrgID: proj.OrgID,
		})
		if errors.Is(connectionErr, pgx.ErrNoRows) {
			writeErr(w, http.StatusBadRequest, "connector_connection_id is not registered in this workspace")
			return
		}
		if connectionErr != nil {
			slogError(w, "get connector connection for alert rule", connectionErr)
			return
		}
		if !strings.EqualFold(connection.Provider, connectorProvider) {
			writeErr(w, http.StatusBadRequest, "connector_connection_id provider does not match connector_provider")
			return
		}
		if !connection.ProjectID.Valid || connection.ProjectID.String != proj.ID {
			writeErr(w, http.StatusBadRequest, "connector_connection_id is not bound to this project")
			return
		}
	} else if req.Type == "connector_sync_health" {
		writeErr(w, http.StatusBadRequest, "connector_sync_health requires a registered connector_connection_id")
		return
	}

	rule, err := s.q.CreateAlertRule(r.Context(), generated.CreateAlertRuleParams{
		ID:                    id.New(),
		ProjectID:             proj.ID,
		OrgID:                 proj.OrgID,
		Name:                  req.Name,
		Type:                  req.Type,
		Threshold:             req.Threshold,
		WindowMinutes:         req.WindowMinutes,
		MetricName:            metricName,
		MetricLabels:          metricLabels,
		Comparison:            comparison,
		ThresholdValue:        thresholdValue,
		SpanName:              spanName,
		Percentile:            percentile,
		ConnectorProvider:     connectorProvider,
		ConnectorOperation:    connectorOperation,
		ConnectorChannel:      connectorChannel,
		ConnectorErrorCode:    connectorErrorCode,
		ConnectorConnectionID: connectorConnectionID,
		ConnectorSourceKind:   connectorSourceKind,
		ConnectorSyncMode:     connectorSyncMode,
		ConnectorQuotaMetric:  connectorQuotaMetric,
		ReleaseVersion:        releaseVersion,
		ServiceName:           serviceName,
		LogSeverity:           logSeverity,
		UsageSurface:          usageSurface,
		UsageOperation:        usageOperation,
		UsageMeasure:          usageMeasure,
		ApiKeyHygieneMode:     apiKeyHygieneMode,
		AiTriageMode:          aiTriageMode,
		BusinessCheckName:     businessCheckName,
		Enabled:               true,
	})
	if err != nil {
		slogError(w, "create alert rule", err)
		return
	}
	s.audit(r.Context(), "alert_rule.create", rule.Type+" on "+proj.Slug)
	writeJSON(w, http.StatusCreated, toAlertRuleResponse(rule))
}

func (s *Server) handleDeleteAlertRule(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.DeleteAlertRule(r.Context(), generated.DeleteAlertRuleParams{
		ID:        chi.URLParam(r, "ruleID"),
		ProjectID: chi.URLParam(r, "id"),
		OrgID:     orgIDFrom(r.Context()),
	})
	if err != nil {
		slogError(w, "delete alert rule", err)
		return
	}
	if rows == 0 {
		writeErr(w, http.StatusNotFound, "alert rule not found")
		return
	}
	s.audit(r.Context(), "alert_rule.delete", chi.URLParam(r, "ruleID"))
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "id")
	rows, err := s.q.DeleteNotificationChannel(r.Context(), generated.DeleteNotificationChannelParams{
		ID:    channelID,
		OrgID: orgIDFrom(r.Context()),
	})
	if err != nil {
		slogError(w, "delete channel", err)
		return
	}
	if rows == 0 {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	s.audit(r.Context(), "channel.delete", channelID)
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleListAlertRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.q.ListAlertRulesByProject(r.Context(), generated.ListAlertRulesByProjectParams{
		ProjectID: chi.URLParam(r, "id"), OrgID: orgIDFrom(r.Context()),
	})
	if err != nil {
		slogError(w, "list alert rules", err)
		return
	}
	out := make([]alertRuleResponse, 0, len(rules))
	for _, rule := range rules {
		out = append(out, toAlertRuleResponse(rule))
	}
	writeJSON(w, http.StatusOK, out)
}

// maxChannelProjects bounds a channel's routing list. A channel realistically
// serves a handful of projects, so 256 is far above any honest fan-out while
// still capping the sequential round trips one PATCH can drive: the 1MB body
// cap in decodeJSON otherwise admits ~40k ids, and the AddChannelProject loop
// issues one INSERT each. De-duplication (below) collapses the "same id 40k
// times" case to one row before this bound is even checked.
const maxChannelProjects = 256

// handleUpdateChannel toggles a channel's enabled flag and its project routing.
//
// notification_channels.enabled has existed since the first schema and the
// dispatch query has always filtered on it, but nothing could ever SET it: the
// routes were create, test and delete. So silencing a channel that started
// paging at 3am meant DELETING it and re-creating it afterwards, re-entering a
// config the API deliberately redacts and will not read back. The column was
// there; only the wiring was missing.
func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled    *bool     `json:"enabled"`
		ProjectIDs *[]string `json:"project_ids"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx := r.Context()
	org := orgIDFrom(ctx)
	chID := chi.URLParam(r, "id")

	// De-duplicate and cap the routing list BEFORE any DB work, so a
	// duplicate-heavy or oversized body is collapsed or rejected before it can
	// drive thousands of INSERTs. Insertion order is preserved for a stable,
	// testable result.
	var projectIDs []string
	if req.ProjectIDs != nil {
		seen := make(map[string]struct{}, len(*req.ProjectIDs))
		projectIDs = make([]string, 0, len(*req.ProjectIDs))
		for _, pid := range *req.ProjectIDs {
			if _, dup := seen[pid]; dup {
				continue
			}
			seen[pid] = struct{}{}
			projectIDs = append(projectIDs, pid)
		}
		if len(projectIDs) > maxChannelProjects {
			writeErr(w, http.StatusBadRequest, "too many project_ids (max 256)")
			return
		}
	}

	// Everything runs in ONE transaction: the ownership checks, the enabled
	// write, the per-project org validation, and the routing replace+insert.
	// All validation happens before the first mutation, so nothing is written
	// unless everything is valid. MVCC hides the DELETE-then-INSERT window from
	// every other reader (so a concurrent dispatch never sees the empty
	// channel_projects state, which reads as "all projects"), and any error
	// rolls the whole PATCH back: a failed update leaves both the original
	// routing and the original enabled flag untouched.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		slogError(w, "update channel: begin tx", err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)

	if req.Enabled != nil {
		rows, err := qtx.SetChannelEnabled(ctx, generated.SetChannelEnabledParams{
			ID: chID, OrgID: org, Enabled: *req.Enabled,
		})
		if err != nil {
			slogError(w, "set channel enabled", err)
			return
		}
		if rows == 0 {
			writeErr(w, http.StatusNotFound, "channel not found")
			return
		}
	}

	if req.ProjectIDs != nil {
		// Verify every project belongs to THIS org before storing it. The join
		// table has no org_id of its own, so this handler is the only thing
		// standing between a routing list and a cross-tenant reference.
		for _, pid := range projectIDs {
			if _, err := qtx.GetProjectByID(ctx, generated.GetProjectByIDParams{ID: pid, OrgScope: org}); err != nil {
				writeErr(w, http.StatusBadRequest, "unknown project in project_ids")
				return
			}
		}
		// Confirm the channel is this org's before touching its routing: without
		// this, an id from another tenant would have its routing rewritten.
		if _, err := qtx.GetNotificationChannel(ctx, generated.GetNotificationChannelParams{ID: chID, OrgID: org}); err != nil {
			writeErr(w, http.StatusNotFound, "channel not found")
			return
		}
		if err := qtx.ReplaceChannelProjects(ctx, chID); err != nil {
			slogError(w, "clear channel routing", err)
			return
		}
		for _, pid := range projectIDs {
			if err := qtx.AddChannelProject(ctx, generated.AddChannelProjectParams{
				ChannelID: chID, ProjectID: pid,
			}); err != nil {
				slogError(w, "set channel routing", err)
				return
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		slogError(w, "update channel: commit", err)
		return
	}

	s.audit(ctx, "channel.update", chID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
