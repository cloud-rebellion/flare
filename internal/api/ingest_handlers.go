package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/ai"
	"github.com/bright-interaction/flare/internal/alerts"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/ingest"
)

const (
	maxIngestBody       = 1 << 20 // 1 MiB compressed request body
	maxDecompressedBody = 8 << 20 // 8 MiB cap after gzip inflation (bomb guard)

	// maxEnvelopeItems caps the RECORDS in one envelope, which the byte caps
	// above do not.
	//
	// An envelope is newline-delimited, so 8 MiB of `{"type":"event"}\n{}\n`
	// pairs is ~400,000 items, and each event item runs UpsertIssue +
	// InsertEvent synchronously. One gzipped ~100 KiB POST therefore buys
	// hundreds of thousands of round trips for ONE rate-limit token, because the
	// limiter charges per REQUEST. That is the amplification: the attacker pays
	// once and the database pays 400,000 times.
	//
	// 1000 is far above any real SDK batch (Sentry clients flush a handful of
	// events per envelope, and a transaction carries its spans inside one item)
	// while capping the per-request work at something a single token can honestly
	// pay for.
	maxEnvelopeItems = 1000
)

// handleEnvelope ingests a Sentry envelope (the modern @sentry/* SDK path).
func (s *Server) handleEnvelope(w http.ResponseWriter, r *http.Request) {
	var eventOutcome, transactionOutcome, spanOutcome, otherOutcome ingestOutcome
	var budget *ingestBudget
	var droppedEvents, droppedTransactions, droppedOther uint64
	var receipt *ingestReceipt
	receiptStatus := http.StatusOK
	requestOutcome := "rejected"
	defer func() {
		if budget != nil {
			spanOutcome.dropped = uint64(budget.dropped)
		}
		eventOutcome.dropped += droppedEvents
		transactionOutcome.dropped += droppedTransactions
		otherOutcome.dropped += droppedOther
		eventOutcome.record("event")
		transactionOutcome.record("transaction")
		spanOutcome.record("span")
		otherOutcome.record("other")
		total := eventOutcome
		total.add(transactionOutcome)
		total.add(spanOutcome)
		total.add(otherOutcome)
		recordIngestRequest("sentry", total, requestOutcome)
		s.finishIngestReceipt(r.Context(), receipt, total, requestOutcome, receiptStatus)
	}()
	project, ok := s.authIngest(w, r)
	if !ok {
		return
	}
	body, ok := readIngestBody(w, r)
	if !ok {
		return
	}
	receipt = s.requireIngestReceipt(r.Context(), w, r, project, "envelope", "sentry", &eventOutcome, &requestOutcome, &receiptStatus)
	if receipt == nil {
		return
	}
	items, err := ingest.ParseEnvelope(body)
	if err != nil {
		receiptStatus = http.StatusBadRequest
		writeErr(w, http.StatusBadRequest, "invalid envelope")
		return
	}
	receipt.setReceived(len(items))
	requestOutcome = ""
	// Truncate rather than reject. A legitimate SDK never comes near this, and
	// answering 4xx would make Sentry clients DISCARD the payload as malformed;
	// accepting the first maxEnvelopeItems keeps real data flowing while the
	// amplification is bounded.
	if len(items) > maxEnvelopeItems {
		for _, item := range items[maxEnvelopeItems:] {
			switch item.Type {
			case "event":
				droppedEvents++
			case "transaction":
				droppedTransactions++
			default:
				droppedOther++
			}
		}
		items = items[:maxEnvelopeItems]
		slog.Warn("envelope truncated: too many items",
			"project_id", project.ID, "kept", maxEnvelopeItems,
			"dropped", droppedEvents+droppedTransactions+droppedOther)
	}

	// One budget for the whole request. maxEnvelopeItems bounds the ITEMS, and on
	// the events path one item is one row, but a transaction item carries its
	// whole span tree: capping per persistSpans call would still let one envelope
	// write maxEnvelopeItems x cap span rows. See maxIngestRecords.
	budget = newIngestBudget()

	var lastID string
	var events, stored int
	// Transactions are counted the same way events are. They were the ONE
	// ingest path that discarded data on a DB error and still answered 200: a
	// Sentry SDK reads 200 as "accepted" and drops the payload, so every
	// database blip permanently lost tracing data on the Sentry-wire path,
	// while the OTLP trace path beside it returned 500 and the client retried.
	// This is the prior audit's "4xx on ingest failure" fix applied to the
	// events arm and not to the arm next to it.
	var transactions, storedTransactions int
	for _, item := range items {
		switch item.Type {
		case "transaction":
			transactions++
			// Sentry-wire tracing: a transaction item carries the root span plus
			// its children. Map to Flare spans and store via the same path OTLP
			// traces use. A persistence error is surfaced as retryable below so a
			// Sentry SDK cannot discard a transaction that was never stored.
			spans, perr := ingest.ParseSentryTransaction(item.Payload)
			if perr != nil {
				transactionOutcome.invalid++
				slog.Warn("envelope transaction parse failed", "project_id", project.ID, "error", perr)
				continue
			}
			storedSpans, perr := s.persistSpans(r.Context(), project, spans, budget)
			if perr != nil {
				transactionOutcome.failed++
				slog.Warn("persist transaction spans failed", "project_id", project.ID, "error", perr)
				continue
			}
			if storedSpans == 0 {
				// The request budget may have admitted no rows after an earlier
				// transaction consumed the cap. Do not tell Sentry that this
				// transaction was accepted when no span row was written.
				transactionOutcome.dropped++
				continue
			}
			storedTransactions++
			transactionOutcome.accepted++
		case "event":
			events++
			eid, ierr := s.ingestOne(r.Context(), project, item.Payload)
			if ierr != nil {
				var parseErr *ingestParseError
				if errors.As(ierr, &parseErr) {
					eventOutcome.invalid++
				} else {
					eventOutcome.failed++
				}
				slog.Warn("envelope event ingest failed", "project_id", project.ID, "error", ierr)
				continue
			}
			stored++
			eventOutcome.accepted++
			lastID = eid
		default:
			// sessions, attachments, client reports: not stored.
			otherOutcome.invalid++
		}
	}
	// A 200 tells the SDK the payload was accepted, so it drops it. Answering
	// 200 when EVERY event in the envelope failed silently discarded the data.
	// 5xx, not 4xx: Sentry SDKs treat 4xx as "this payload is bad, discard it"
	// and only retry/back off on 5xx, and a total failure here is a server-side
	// problem, not a malformed request. Partial success still returns 200,
	// because the SDK cannot retry individual items.
	if events > 0 && stored == 0 {
		if eventOutcome.failed > 0 {
			requestOutcome = "failed"
			receiptStatus = http.StatusServiceUnavailable
			writeErr(w, http.StatusServiceUnavailable, "no event in the envelope could be stored")
			return
		}
		requestOutcome = "rejected"
		receiptStatus = http.StatusBadRequest
		writeErr(w, http.StatusBadRequest, "every event in the envelope was invalid")
		return
	}
	// Same rule for an envelope that carried only transactions. A parse failure
	// is the client's problem and is not counted here; a persist failure is
	// ours, and telling the SDK "accepted" is how the span tree is lost.
	if events == 0 && transactions > 0 && storedTransactions == 0 {
		if transactionOutcome.failed > 0 {
			requestOutcome = "failed"
			receiptStatus = http.StatusServiceUnavailable
			writeErr(w, http.StatusServiceUnavailable, "no transaction in the envelope could be stored")
			return
		}
		requestOutcome = "rejected"
		receiptStatus = http.StatusBadRequest
		writeErr(w, http.StatusBadRequest, "every transaction in the envelope was invalid")
		return
	}
	budget.report(project.ID, "spans")
	writeJSON(w, http.StatusOK, map[string]string{"id": lastID})
}

// handleStore ingests a single event JSON (Sentry /store/ and Flare's native
// /events endpoint).
func (s *Server) handleStore(w http.ResponseWriter, r *http.Request) {
	var outcome ingestOutcome
	var receipt *ingestReceipt
	receiptStatus := http.StatusOK
	requestOutcome := "rejected"
	defer func() {
		outcome.record("event")
		recordIngestRequest("sentry", outcome, requestOutcome)
		s.finishIngestReceipt(r.Context(), receipt, outcome, requestOutcome, receiptStatus)
	}()
	project, ok := s.authIngest(w, r)
	if !ok {
		return
	}
	body, ok := readIngestBody(w, r)
	if !ok {
		return
	}
	receipt = s.requireIngestReceipt(r.Context(), w, r, project, "event", "sentry", &outcome, &requestOutcome, &receiptStatus)
	if receipt == nil {
		return
	}
	receipt.setReceived(1)
	eid, err := s.ingestOne(r.Context(), project, body)
	if err != nil {
		// Distinguish a client PARSE failure from a server-side store/DB failure.
		// Sentry SDKs treat 4xx as "malformed, drop it" and only retry on 5xx, so
		// answering 400 on a DB blip silently loses a retryable event. Mirror
		// handleEnvelope: 4xx only for a bad body, 503 for a store failure.
		var perr *ingestParseError
		if errors.As(err, &perr) {
			outcome.invalid = 1
			receiptStatus = http.StatusBadRequest
			writeErr(w, http.StatusBadRequest, "invalid event")
			return
		}
		outcome.failed = 1
		requestOutcome = "failed"
		receiptStatus = http.StatusServiceUnavailable
		slog.Warn("store event failed", "project_id", project.ID, "error", err)
		writeErr(w, http.StatusServiceUnavailable, "could not store event")
		return
	}
	outcome.accepted = 1
	requestOutcome = ""
	writeJSON(w, http.StatusOK, map[string]string{"id": eid})
}

// ingestParseError marks an ingestOne failure as a client-side parse problem (a
// malformed event body) as opposed to a server-side store/DB failure. handleStore
// maps it to 4xx; every other error is a store failure and maps to 5xx so the SDK
// retries instead of discarding the event.
type ingestParseError struct{ err error }

func (e *ingestParseError) Error() string { return e.err.Error() }
func (e *ingestParseError) Unwrap() error { return e.err }

// ingestOne normalizes, groups, and persists one event. Returns the event id.
// A parse failure is wrapped in *ingestParseError; a store failure is returned
// bare, so callers can tell client errors from server errors.
func (s *Server) ingestOne(ctx context.Context, project *generated.Project, raw []byte) (string, error) {
	ev, err := ingest.ParseEvent(raw)
	if err != nil {
		return "", &ingestParseError{err}
	}

	// Sanitize every client-controlled field before it reaches Postgres. This is
	// the guard the logs/spans/metrics handlers already apply and the events path
	// alone was missing: Postgres rejects a NUL byte (raw or the backslash-u-0000
	// escape) and invalid UTF-8 in a TEXT or JSONB column, so an unsanitized field
	// fails the INSERT and the SDK then DROPS the event (silent per-event data
	// loss). SanitizeText strips those from TEXT fields; SanitizeJSON parse-
	// remarshals the JSONB payload and stacktrace so an escaped NUL is removed
	// without corrupting the document.
	msg := ingest.SanitizeText(ev.Message)
	excType := ingest.SanitizeText(ev.ExceptionType)
	excValue := ingest.SanitizeText(ev.ExceptionValue)
	title := ingest.SanitizeText(ev.Title)
	culprit := ingest.SanitizeText(ev.Culprit)
	level := ingest.SanitizeText(ev.Level)
	platform := ingest.SanitizeText(ev.Platform)
	environment := ingest.SanitizeText(ev.Environment)
	release := ingest.SanitizeText(ev.Release)
	traceID := ingest.SanitizeText(ev.TraceID)
	spanID := ingest.SanitizeText(ev.SpanID)
	payload := ingest.SanitizeJSON(ev.Raw)
	stacktrace := ingest.SanitizeJSON(stacktraceJSON(ev.Frames))
	eventID := ev.EventID
	if eventID == "" {
		eventID = id.New()
	}

	// Auto-track the release this event reports (deploy markers can also
	// register one explicitly). Best-effort: a failure never blocks ingest.
	if release != "" {
		_ = s.q.UpsertRelease(ctx, generated.UpsertReleaseParams{
			ID: id.New(), ProjectID: project.ID, OrgID: project.OrgID, Version: release,
		})
	}

	// An accepted event must carry a durable alert-evaluation handoff. Keep
	// issue grouping, the event, its release marker, and the queue row in one
	// transaction so a queue failure returns 503 without persisting a phantom
	// event or spending the new-issue/regression transition.
	if s.pool == nil {
		return "", errors.New("event transaction unavailable")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin event transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)
	issue, err := qtx.UpsertIssue(ctx, generated.UpsertIssueParams{
		ID:           id.New(),
		ProjectID:    project.ID,
		OrgID:        project.OrgID,
		Fingerprint:  ev.Fingerprint(),
		Title:        title,
		Culprit:      culprit,
		Level:        level,
		Platform:     platform,
		FirstRelease: release,
	})
	if err != nil {
		return "", err
	}

	// UpsertIssue leaves status untouched, so issue.Status is the PRIOR status.
	// A resolved issue that recurs is a regression: reopen it here.
	reopened := false
	if !issue.IsNew && issue.Status == "resolved" {
		if err := qtx.ReopenIssue(ctx, generated.ReopenIssueParams{ID: issue.ID, OrgID: project.OrgID}); err != nil {
			return "", fmt.Errorf("reopen issue: %w", err)
		}
		reopened = true
	}

	insertParams := generated.InsertEventParams{
		ID:             id.New(),
		ProjectID:      project.ID,
		OrgID:          project.OrgID,
		IssueID:        pgText(issue.ID),
		Level:          level,
		Message:        msg,
		ExceptionType:  excType,
		ExceptionValue: excValue,
		Platform:       platform,
		Environment:    environment,
		Release:        release,
		Stacktrace:     stacktrace,
		Payload:        payload,
		TraceID:        textOrNull(traceID),
		SpanID:         textOrNull(spanID),
	}
	// A release freshness signal is deliberately narrower than generic
	// activity: only the built-in info-level service-up:<service> heartbeat,
	// carrying a non-empty release, advances the release heartbeat timestamp.
	// This keeps arbitrary event traffic from making a deployed release look
	// live while retaining the payload-free event record as the source of truth.
	if err := qtx.InsertEvent(ctx, insertParams); err != nil {
		return "", err
	}
	if release != "" && isReleaseHeartbeat(level, msg) {
		rows, err := qtx.TouchReleaseHeartbeat(ctx, generated.TouchReleaseHeartbeatParams{
			ProjectID: project.ID, OrgID: project.OrgID, Version: release,
		})
		if err != nil {
			return "", fmt.Errorf("persist release heartbeat: %w", err)
		}
		if rows != 1 {
			return "", fmt.Errorf("persist release heartbeat: release %q is not registered", release)
		}
	}

	// Sensitive-data scan: flag the issue and surface a security signal when an
	// event leaked a secret / JWT / card number. Skipped for the security
	// project itself, both to avoid recursion and because its own event bodies
	// (key prefixes, IPs) are not leaks.
	sensitive := ""
	if project.Slug != securityProjectSlug {
		if kinds := detectSensitive(ev); len(kinds) > 0 {
			sensitive = strings.Join(kinds, ",")
			if err := qtx.MarkIssueSensitive(ctx, generated.MarkIssueSensitiveParams{
				ID: issue.ID, OrgID: project.OrgID, Sensitive: sensitive,
			}); err != nil {
				return "", fmt.Errorf("mark sensitive issue: %w", err)
			}
			// The security event is itself re-ingested as an event into the
			// flare-security project, so its message must carry the SCRUBBED
			// title. Passing issue.Title raw copied the detected secret into a
			// project the detector deliberately skips, which round-tripped the
			// leak straight past the redaction.
		}
	}

	// Low-severity issues are retained as telemetry but never page. A queue
	// row is needed only while at least one relevant issue rule is enabled.
	if issue.Status != "ignored" && pageableLevel(issue.Level) {
		var enabled bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM alert_rules
			WHERE org_id=$1 AND project_id=$2 AND enabled
			  AND (type='spike' OR (type='new_issue' AND $3::boolean)
		       OR (type='regression' AND $4::boolean))
		)`, project.OrgID, project.ID, issue.IsNew, reopened).Scan(&enabled); err != nil {
			return "", fmt.Errorf("check issue alert rules: %w", err)
		}
		if enabled {
			if _, err := tx.Exec(ctx, `INSERT INTO issue_alert_evaluations
				(id,org_id,project_id,issue_id,is_new,reopened,event_count,observed_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,now())`, insertParams.ID, project.OrgID, project.ID, issue.ID, issue.IsNew, reopened,
				max(int64(0), min(issue.EventCount, int64(1_000_000_000)))); err != nil {
				return "", fmt.Errorf("enqueue issue alert evaluation: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit event and alert evaluation: %w", err)
	}
	if sensitive != "" {
		s.recordSecurityEvent(project.OrgID, "sensitive-data-in-payload",
			"possible "+sensitive+" in "+project.Name+" - issue: "+ai.Scrub(issue.Title), "")
	}

	// A newly-seen issue gets AI triage automatically (background, budget-gated),
	// so its root cause and fix are already waiting when a human or the AI
	// operator opens it - not a manual step after the fact.
	if issue.IsNew {
		s.maybeAutoTriage(project.OrgID, issue.ID)
	}
	return eventID, nil
}

func isReleaseHeartbeat(level, message string) bool {
	if level != "info" {
		return false
	}
	service := strings.TrimSpace(strings.TrimPrefix(message, "service-up:"))
	return strings.HasPrefix(message, "service-up:") && service != ""
}

// pageableLevel reports whether an issue at this severity may fire an alert.
// info/debug are informational (heartbeats, breadcrumbs) and never page; every
// other level - including the empty/default, which Sentry treats as error -
// does. Kept a pure fn so the gate is unit-tested.
func pageableLevel(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "info", "debug":
		return false
	default:
		return true
	}
}

// evaluateAlerts runs one durable evaluation. Database and automation enqueue
// failures are returned so the leased job can retry; intentional suppression
// (ignored issue, low severity, no rule/channel, rate cap) is terminal.
func (s *Server) evaluateAlerts(ctx context.Context, project *generated.Project, issue *generated.UpsertIssueRow, isNew, reopened bool, evaluationID, leaseToken string, observedAt time.Time) error {
	// "Ignore" is the only control the UI offers for a noisy issue, and it hid
	// the issue from the list while its alerts kept firing, so the button did
	// not do the one thing users press it for. issue.Status is the PRIOR status
	// from UpsertIssue; a reopened issue was 'resolved', never 'ignored'.
	if issue.Status == "ignored" {
		return nil
	}
	// Low-severity signals (info/debug: heartbeats, breadcrumbs, health check-ins)
	// never page, whatever the rules say - they are informational, not incidents.
	// They still ingest and count toward the watchdog's silence-detection activity
	// baseline (CountEventsForProjectSince is level-agnostic), so a liveness
	// heartbeat can keep a project "active" without ever firing a new-issue/spike
	// alert. Warning+ (and the default/empty level) alert as before.
	if !pageableLevel(issue.Level) {
		return nil
	}
	{
		rules, err := s.q.ListEnabledAlertRulesByProject(ctx, generated.ListEnabledAlertRulesByProjectParams{
			ProjectID: project.ID,
			OrgID:     project.OrgID,
		})
		if err != nil {
			return fmt.Errorf("list issue alert rules: %w", err)
		}
		if len(rules) == 0 {
			return nil
		}
		// regression and new_issue are BOOLEAN rules: they either apply to this
		// event or they do not, so collapsing duplicates of those is harmless.
		byType := make(map[string]*generated.AlertRule, len(rules))
		// Spike rules are NOT interchangeable and must each be evaluated.
		//
		// Collapsing them to the "most sensitive" one silently disabled every
		// other spike rule on the project. moreSensitiveRule ranks on threshold
		// first, so "10 in 5min" beat "50 in 1440min" and the second never ran.
		// Those detect different things: a burst, and a slow burn that never
		// reaches 10 within any 5-minute window. An operator who configured both
		// got one, with nothing in the UI saying so.
		var spikes []*generated.AlertRule
		for _, r := range rules {
			if r.Type == "spike" {
				spikes = append(spikes, r)
				continue
			}
			if cur, ok := byType[r.Type]; !ok || moreSensitiveRule(r, cur) {
				byType[r.Type] = r
			}
		}
		// Deterministic order, most sensitive first, so the reason reported when
		// several rules fire at once is stable across runs and is the tightest
		// one that matched.
		sort.SliceStable(spikes, func(i, j int) bool { return moreSensitiveRule(spikes[i], spikes[j]) })

		reason := ""
		if reopened && byType["regression"] != nil {
			reason = "Regression"
		}
		// Determine threshold matches without spending the spike claim. The
		// claim is written later in the same transaction as automation notices.
		var spikeCandidates []spikeAlertCandidate
		if reason == "" {
			for _, sr := range spikes {
				candidate, err := s.spikeReason(ctx, issue, project.OrgID, sr, observedAt)
				if err != nil {
					return err
				}
				if candidate.reason != "" {
					spikeCandidates = append(spikeCandidates, candidate)
				}
			}
		}
		newIssueAlert := isNew && byType["new_issue"] != nil
		if reason == "" && len(spikeCandidates) == 0 && !newIssueAlert {
			return nil
		}

		// Per-org volume cap, matching the monitor path. Checked AFTER the
		// reason is settled so an event that was never going to alert does not
		// consume the org's budget, and BEFORE the channel load so a throttled
		// alert costs one query rather than a fan-out.
		if !s.issueAlertLimiter.Allow("issue-alert-org:" + project.OrgID) {
			slog.Warn("issue alert throttled: org is over its per-minute alert budget",
				"org_id", project.OrgID, "project_id", project.ID, "issue_id", issue.ID)
			return nil
		}

		// Respect per-project routing: an issue alert is about this project.
		chans, err := s.q.ListEnabledChannelsForProject(ctx, generated.ListEnabledChannelsForProjectParams{
			OrgID: project.OrgID, ProjectID: project.ID,
		})
		if err != nil {
			return fmt.Errorf("list issue alert channels: %w", err)
		}
		if len(chans) == 0 {
			return nil
		}
		channels := make([]alerts.Channel, 0, len(chans))
		for _, c := range chans {
			channels = append(channels, alerts.Channel{ID: c.ID, OrgID: c.OrgID, Type: c.Type, Config: s.decryptChannelConfig(c.Type, c.Config)})
		}
		// Alerts leave the tenant boundary: three of the four channel types
		// (webhook, slack, email) deliver to a third party. So the scrub is
		// UNCONDITIONAL, the way recordSecurityEvent already does it.
		//
		// It used to be gated on `sensitive != ""`, which is the detector's
		// verdict, and the detector is strictly narrower than the scrubber it
		// then calls. Seven of eight measured shapes walked straight through:
		// a postgres URL with credentials, an Authorization header, an
		// assignment-style secret, a plain database password, a customer email,
		// a customer IP, a GitLab PAT. Every one of them is a string an
		// anonymous holder of a DSN public key can put in an exception message,
		// so the gate turned "catch a leaked secret" into "forward an arbitrary
		// string to an endpoint the attacker chose". The comment two lines up
		// said exactly what would happen; the condition made it happen.
		//
		// detectSensitive decides whether to raise the badge. It never decides
		// whether redaction runs.
		notice := alerts.Notification{
			ProjectID:            project.ID,
			ProjectName:          project.Name,
			IssueID:              issue.ID,
			Title:                ai.Scrub(issue.Title),
			Level:                issue.Level,
			Culprit:              ai.Scrub(issue.Culprit),
			EventCount:           issue.EventCount,
			EvaluationID:         evaluationID,
			EvaluationLeaseToken: leaseToken,
			ObservedAt:           observedAt,
			URL:                  strings.TrimRight(s.cfg.BaseURL, "/") + "/issues/" + issue.ID,
		}
		if reason != "" {
			notice.Reason = reason
			if _, err := s.dispatchWithAutomationClaim(ctx, project.OrgID, project.ID, channels, notice, nil); err != nil {
				return fmt.Errorf("queue regression automation alert: %w", err)
			}
			return nil
		}
		for _, candidate := range spikeCandidates {
			notice.Reason = candidate.reason
			claimed, err := s.dispatchWithAutomationClaim(ctx, project.OrgID, project.ID, channels, notice,
				func(q *generated.Queries) (int64, error) {
					return q.TrySetIssueSpike(ctx, generated.TrySetIssueSpikeParams{
						ID: issue.ID, OrgID: project.OrgID, LastSpikeAt: candidate.cutoff,
					})
				})
			if err != nil {
				return fmt.Errorf("claim/queue spike alert: %w", err)
			}
			if claimed {
				return nil
			}
		}
		if newIssueAlert {
			notice.Reason = "New issue"
			if _, err := s.dispatchWithAutomationClaim(ctx, project.OrgID, project.ID, channels, notice, nil); err != nil {
				return fmt.Errorf("queue new-issue automation alert: %w", err)
			}
		}
	}
	return nil
}

// moreSensitiveRule reports whether a would fire earlier than b: a lower
// threshold first, then a longer window at the same threshold. Kept pure so the
// tie-break is testable.
func moreSensitiveRule(a, b *generated.AlertRule) bool {
	if a.Threshold != b.Threshold {
		return a.Threshold < b.Threshold
	}
	return a.WindowMinutes > b.WindowMinutes
}

type spikeAlertCandidate struct {
	reason string
	cutoff pgtype.Timestamptz
}

// spikeReason evaluates the threshold only. Its claim is made by the caller in
// the same transaction as the durable automation enqueue.
func (s *Server) spikeReason(ctx context.Context, issue *generated.UpsertIssueRow, org string, rule *generated.AlertRule, observedAt time.Time) (spikeAlertCandidate, error) {
	if rule.Threshold < 1 || rule.WindowMinutes < 1 {
		return spikeAlertCandidate{}, nil
	}
	// A delayed job must evaluate the window at the accepted event's time.
	// Counting later events would turn an old new-issue job into a fresh spike
	// and make the selected reason depend on worker backlog.
	cutoff := pgtype.Timestamptz{Time: observedAt.Add(-time.Duration(rule.WindowMinutes) * time.Minute), Valid: true}
	var cnt int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM events
		WHERE issue_id=$1 AND org_id=$2 AND project_id=$3
		  AND received_at >= $4 AND received_at <= $5`,
		issue.ID, org, issue.ProjectID, cutoff.Time, observedAt).Scan(&cnt)
	if err != nil {
		return spikeAlertCandidate{}, fmt.Errorf("count issue spike events: %w", err)
	}
	if cnt < int64(rule.Threshold) {
		return spikeAlertCandidate{}, nil
	}
	return spikeAlertCandidate{reason: fmt.Sprintf("Spike: %d events in %dm", cnt, rule.WindowMinutes), cutoff: cutoff}, nil
}

// authIngest resolves the project from the DSN public key (the ingest secret)
// and sanity-checks the {projectID} path segment when present. The path id in a
// Sentry DSN is the numeric dsn_id; we also accept the cuid so native/Flare-key
// clients that use the primary key keep working. The public_key is the actual
// auth, so a mismatched-but-present path id is not by itself fatal.
func (s *Server) authIngest(w http.ResponseWriter, r *http.Request) (*generated.Project, bool) {
	key := ingestKey(r)
	if key == "" {
		slog.Warn("ingest rejected: missing key", "path", r.URL.Path, "remote", remoteIP(r))
		writeErr(w, http.StatusUnauthorized, "missing ingest key")
		return nil, false
	}
	project, err := s.q.GetProjectByPublicKey(r.Context(), key)
	if err != nil {
		// Log the key PREFIX only: enough to identify a stale/deleted DSN in
		// the field (the exact failure mode after a project deletion, which
		// used to be fully silent) without leaking a usable credential.
		slog.Warn("ingest rejected: unknown key", "key_prefix", keyPrefix(key), "path", r.URL.Path, "remote", remoteIP(r))
		s.recordSecurityEvent("", "ingest-auth-rejected", "unknown ingest key "+keyPrefix(key)+" at "+r.URL.Path, remoteIP(r))
		writeErr(w, http.StatusUnauthorized, "invalid ingest key")
		return nil, false
	}
	if pid := chi.URLParam(r, "projectID"); pid != "" && pid != project.DsnID && pid != project.ID {
		slog.Warn("ingest rejected: key/project mismatch", "key_prefix", keyPrefix(key), "path_project", pid, "remote", remoteIP(r))
		s.recordSecurityEvent(project.OrgID, "ingest-key-project-mismatch", "key "+keyPrefix(key)+" used against project "+pid, remoteIP(r))
		writeErr(w, http.StatusUnauthorized, "key does not match project")
		return nil, false
	}
	// Record the authenticated DSN request at the boundary. The payload-free
	// ingest receipt later records acceptance/rejection counts; this usage
	// bucket also covers check-ins, which do not create a receipt row.
	s.recordUsage(r.Context(), usageDelta{
		OrgID: project.OrgID, ProjectID: project.ID,
		Surface: "dsn", Operation: dsnUsageOperation(r), Requests: 1,
	})
	return project, true
}

// keyPrefix returns the first 8 chars of an ingest key for diagnostics.
func keyPrefix(key string) string {
	if len(key) > 8 {
		return key[:8]
	}
	return key
}

// remoteIP is the peer address without the port, for rejected-ingest logs.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ingestKey pulls the public key from the X-Sentry-Auth header, the
// sentry_key query param, or the X-Flare-Key header.
// maxIngestKeyLen bounds a DSN public key. Real keys are id.New() cuids (~25
// chars); anything longer is not a key we could ever resolve.
const maxIngestKeyLen = 64

// plausibleIngestKey rejects anything that cannot be a DSN public key. The
// ingest rate limiter runs BEFORE authentication and buckets on this value, so
// without a shape check an attacker can mint unlimited distinct limiter keys
// (each an arbitrary-length header) and grow the limiter map without bound.
// Implausible values return "" so the request falls back to the IP bucket.
func plausibleIngestKey(k string) string {
	if k == "" || len(k) > maxIngestKeyLen {
		return ""
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return ""
		}
	}
	return k
}

func ingestKey(r *http.Request) string {
	return plausibleIngestKey(rawIngestKey(r))
}

func rawIngestKey(r *http.Request) string {
	if h := r.Header.Get("X-Sentry-Auth"); h != "" {
		// Header looks like: "Sentry sentry_key=KEY, sentry_version=7, ...".
		// Split on both spaces and commas so the leading scheme word and the
		// comma-separated pairs are all individual tokens.
		fields := strings.FieldsFunc(h, func(c rune) bool { return c == ',' || c == ' ' })
		for _, f := range fields {
			if v, ok := strings.CutPrefix(f, "sentry_key="); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	if k := r.URL.Query().Get("sentry_key"); k != "" {
		return k
	}
	return strings.TrimSpace(r.Header.Get("X-Flare-Key"))
}

func readIngestBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	var reader io.Reader = http.MaxBytesReader(w, r.Body, maxIngestBody)
	if strings.Contains(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid gzip body")
			return nil, false
		}
		defer gz.Close()
		// Cap the DECOMPRESSED size too: a 1 MiB gzip can inflate to gigabytes
		// (decompression bomb). Read one byte past the cap so overflow is caught.
		reader = io.LimitReader(gz, maxDecompressedBody+1)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "body too large")
		return nil, false
	}
	if len(body) > maxDecompressedBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "body too large")
		return nil, false
	}
	return body, true
}

func stacktraceJSON(frames []ingest.Frame) json.RawMessage {
	if len(frames) == 0 {
		return nil
	}
	b, err := json.Marshal(map[string]any{"frames": frames})
	if err != nil {
		return nil
	}
	return b
}
