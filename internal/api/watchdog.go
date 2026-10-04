package api

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/alerts"
	"github.com/bright-interaction/flare/internal/db/generated"
)

// anomalyMinEvents is the absolute floor for an anomaly alert. Without it, a
// normally-silent project whose baseline is ~0 would look like an infinite
// deviation on its first few errors and page constantly. A real anomaly worth
// waking someone for clears this AND the baseline multiplier.
const anomalyMinEvents = 5

// monitorRenotifySeconds is how long a monitor stays claimed as "missing"
// before it is eligible to page again. Without a re-notify window, the claim is
// taken before dispatch, so one dropped delivery silences a dead cron forever.
const monitorRenotifySeconds int64 = 24 * 60 * 60

// RunWatchdog periodically evaluates the "anomaly" and "silence" alert rules,
// which cannot ride on ingest: silence is the ABSENCE of events, and anomaly
// needs a baseline window. It reuses the same notification channels + dispatch
// as issue alerts. Best-effort throughout: a bad rule logs and is skipped, the
// loop never dies.
func (s *Server) RunWatchdog(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	slog.Info("watchdog started", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// The tick runs in its own goroutine guarded by an in-flight flag, so a slow
	// or hung dispatch (mail, webhook, Slack) can never park the ticker itself.
	// Overlapping ticks are skipped rather than queued: the work is idempotent
	// and claim-gated, so skipping is always safer than piling up.
	var inFlight atomic.Bool
	var skipped atomic.Int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !inFlight.CompareAndSwap(false, true) {
				if n := skipped.Add(1); n%10 == 1 {
					slog.Warn("watchdog: previous tick still running, skipping", "skipped_total", n)
				}
				continue
			}
			go func() {
				defer inFlight.Store(false)
				// A panic in a detached goroutine takes the whole process down,
				// and this one is the alerting loop. Contain it.
				defer func() {
					if r := recover(); r != nil {
						slog.Error("watchdog: tick panicked", "panic", r)
					}
				}()
				s.watchdogTick(ctx)
			}()
		}
	}
}

func (s *Server) watchdogTick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Cache channels per org so N rules/monitors in one org do one channel load.
	chanCache := map[string][]alerts.Channel{}
	now := time.Now()

	// Anomaly/silence rules. A list error is logged but not fatal to the tick:
	// monitor checking below is independent and must still run.
	rules, err := s.q.ListWatchdogRules(ctx)
	if err != nil {
		slog.Error("watchdog: list rules failed", "error", err)
	}

	for _, rule := range rules {
		reason := s.watchdogReason(ctx, rule, now)
		if reason == "" {
			continue
		}
		// Load channels BEFORE claiming the cooldown. A firing condition on an
		// org with no channel (or a transient channel-load error) must not
		// spend the cooldown: otherwise the first real alert is suppressed for
		// up to an hour after a channel is finally added.
		channels := s.projectChannels(ctx, rule.OrgID, rule.ProjectID, chanCache)
		if len(channels) == 0 {
			continue
		}
		// Cooldown dedup: claim the fire only once a notification will actually
		// be dispatched. max(window, 60m) so a persisting condition pages once.
		cooldown := time.Duration(maxInt32(rule.WindowMinutes, 60)) * time.Minute
		claimed, err := s.dispatchWithAutomationClaim(ctx, rule.OrgID, rule.ProjectID, channels, alerts.Notification{
			ProjectID:   rule.ProjectID,
			ProjectName: rule.ProjectName,
			Title:       rule.ProjectName,
			Level:       "warning",
			Reason:      reason,
			URL:         strings.TrimRight(s.cfg.BaseURL, "/") + "/projects/" + rule.ProjectID,
		}, func(q *generated.Queries) (int64, error) {
			return q.TrySetAlertRuleFired(ctx, generated.TrySetAlertRuleFiredParams{
				ID: rule.ID, OrgID: rule.OrgID,
				LastFiredAt: pgtype.Timestamptz{Time: now.Add(-cooldown), Valid: true},
			})
		})
		if err != nil {
			slog.Error("watchdog: claim/queue failed", "rule_id", rule.ID, "error", err)
			continue
		}
		if !claimed {
			continue
		}
		slog.Info("watchdog: alert fired", "type", rule.Type, "project", rule.ProjectName, "reason", reason)
	}

	// Overdue cron/check-in monitors: a scheduled job that stopped pinging.
	s.checkMonitors(ctx, chanCache, now)
}

// checkMonitors fires a "missing" alert for each configured monitor whose
// check-in is overdue (interval + grace elapsed). Claims each atomically via
// TrySetMonitorMissing so exactly one replica pages, and only once until the
// job checks in again (which flips state back to ok).
func (s *Server) checkMonitors(ctx context.Context, chanCache map[string][]alerts.Channel, now time.Time) {
	due, err := s.q.ListDueMonitors(ctx, generated.ListDueMonitorsParams{
		Column1:         pgtype.Timestamptz{Time: now, Valid: true},
		RenotifySeconds: monitorRenotifySeconds,
	})
	if err != nil {
		slog.Error("watchdog: list due monitors failed", "error", err)
		return
	}
	for _, m := range due {
		channels := s.projectChannels(ctx, m.OrgID, m.ProjectID, chanCache)
		if len(channels) == 0 {
			continue
		}
		// Mirror the query's COALESCE: a monitor that has never checked in is
		// measured from creation, and says so, rather than reporting a nonsense
		// age off a zero-value timestamp.
		since, reason := m.LastPingAt.Time, "Check-in missing"
		if !m.LastPingAt.Valid {
			since, reason = m.CreatedAt.Time, "Never checked in since it was created"
		}
		overdue := int64(now.Sub(since).Seconds())
		claimed, err := s.dispatchWithAutomationClaim(ctx, m.OrgID, m.ProjectID, channels, alerts.Notification{
			ProjectID:   m.ProjectID,
			ProjectName: m.ProjectName,
			Title:       "Monitor overdue: " + m.Slug,
			Level:       "warning",
			Reason:      fmt.Sprintf("%s: nothing for ~%ds (expected every %ds + %ds grace)", reason, overdue, m.IntervalSeconds, m.GraceSeconds),
			URL:         strings.TrimRight(s.cfg.BaseURL, "/") + "/projects/" + m.ProjectID,
		}, func(q *generated.Queries) (int64, error) {
			return q.TrySetMonitorMissing(ctx, generated.TrySetMonitorMissingParams{
				ID: m.ID, OrgID: m.OrgID,
				LastAlertAt:     pgtype.Timestamptz{Time: now, Valid: true},
				RenotifySeconds: monitorRenotifySeconds,
			})
		})
		if err != nil {
			slog.Error("watchdog: monitor claim/queue failed", "monitor_id", m.ID, "error", err)
			continue
		}
		if !claimed {
			continue
		}
		slog.Info("watchdog: monitor missing", "monitor", m.Slug, "project", m.ProjectName)
	}
}

// orgChannels returns an org's enabled notification channels, cached per tick.
// A load error caches an empty slice so the rule is skipped (never dispatched
// to nothing, never a spent cooldown) and retried on the next tick.
// projectChannels resolves the channels for one project, honouring routing.
//
// The cache key is (org, project), not org: watchdog alerts are per project, so
// caching by org alone would hand project B the channel set computed for project
// A and silently defeat routing on every alert after the first in a tick.
func (s *Server) projectChannels(ctx context.Context, org, project string, cache map[string][]alerts.Channel) []alerts.Channel {
	key := org + "|" + project
	if channels, ok := cache[key]; ok {
		return channels
	}
	chans, err := s.q.ListEnabledChannelsForProject(ctx, generated.ListEnabledChannelsForProjectParams{
		OrgID: org, ProjectID: project,
	})
	if err != nil {
		slog.Error("watchdog: load channels failed", "org", org, "project", project, "error", err)
		cache[key] = nil
		return nil
	}
	channels := make([]alerts.Channel, 0, len(chans))
	for _, c := range chans {
		channels = append(channels, alerts.Channel{ID: c.ID, OrgID: c.OrgID, Type: c.Type, Config: s.decryptChannelConfig(c.Type, c.Config)})
	}
	cache[key] = channels
	return channels
}

// watchdogReason evaluates one rule against live counts and returns a non-empty
// reason when it should fire. It fetches bounded, tenant-scoped aggregates and
// then defers to pure decision functions (tested without a DB).
// The two rule types deliberately count DIFFERENT things:
//
//	anomaly  counts only actionable events (error/warning/fatal). An
//	         "error-rate anomaly" that counted info-level heartbeats would fire
//	         on liveness traffic, which is exactly what happened: the whole
//	         estate tripped it on 2026-07-11 when the heartbeat shipped, and
//	         mesh-hub + mesh-cloud tripped it again the moment they were first
//	         instrumented (0 events -> a steady beacon reads as an infinite
//	         spike against the project's own zero baseline).
//
//	silence  counts EVERY event, heartbeats included. That is the whole point:
//	         the beacon is what proves a healthy, error-free service is alive, so
//	         filtering it out would make every quiet-but-healthy service look dead.
func (s *Server) watchdogReason(ctx context.Context, rule *generated.ListWatchdogRulesRow, now time.Time) string {
	if rule.WindowMinutes < 1 {
		return ""
	}
	windowStart := now.Add(-time.Duration(rule.WindowMinutes) * time.Minute)
	dayStart := now.Add(-24 * time.Hour)

	switch rule.Type {
	case "anomaly":
		recent, err := s.countActionableEventsSince(ctx, rule.ProjectID, rule.OrgID, windowStart)
		if err != nil {
			// Swallowing this silently disabled every anomaly and silence alert
			// for as long as the count query kept failing, with nothing in the
			// logs to say so.
			slog.Error("watchdog: baseline count failed, rule skipped",
				"rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		day, err := s.countActionableEventsSince(ctx, rule.ProjectID, rule.OrgID, dayStart)
		if err != nil {
			slog.Error("watchdog: baseline count failed, rule skipped",
				"rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return anomalyReason(recent, day, rule.WindowMinutes, rule.Threshold)
	case "silence":
		recent, err := s.countEventsSince(ctx, rule.ProjectID, rule.OrgID, windowStart)
		if err != nil {
			slog.Error("watchdog: baseline count failed, rule skipped",
				"rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		day, err := s.countEventsSince(ctx, rule.ProjectID, rule.OrgID, dayStart)
		if err != nil {
			slog.Error("watchdog: baseline count failed, rule skipped",
				"rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return silenceReason(recent, day, rule.Threshold)
	case "metric_threshold":
		stats, err := s.q.GetMetricThresholdStats(ctx, generated.GetMetricThresholdStatsParams{
			ProjectID:    rule.ProjectID,
			OrgID:        rule.OrgID,
			Name:         rule.MetricName,
			MetricLabels: rule.MetricLabels,
			ObservedAt:   pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: metric threshold query failed, rule skipped",
				"rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return metricThresholdReason(stats, rule.MetricName, rule.Comparison, rule.ThresholdValue, rule.WindowMinutes)
	case "trace_percentile":
		stats, err := s.q.GetTracePercentileStats(ctx, generated.GetTracePercentileStatsParams{
			ProjectID:  rule.ProjectID,
			OrgID:      rule.OrgID,
			SpanName:   rule.SpanName,
			Percentile: float64(rule.Percentile) / 100,
			Since:      pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: trace percentile query failed, rule skipped",
				"rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return tracePercentileReason(stats, rule.SpanName, rule.Percentile, rule.Comparison, rule.ThresholdValue, rule.WindowMinutes)
	case "service_level":
		stats, err := s.q.GetServiceLevelStats(ctx, generated.GetServiceLevelStatsParams{
			ProjectID: rule.ProjectID,
			OrgID:     rule.OrgID,
			SpanName:  rule.SpanName,
			LatencyMs: float64(rule.Threshold),
			Since:     pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: service-level query failed, rule skipped", "rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return serviceLevelReason(stats, rule.SpanName, rule.Threshold, rule.ThresholdValue, rule.WindowMinutes)
	case "business_health":
		stats, err := s.q.GetBusinessHealthStats(ctx, generated.GetBusinessHealthStatsParams{
			ProjectID: rule.ProjectID,
			OrgID:     rule.OrgID,
			CheckName: rule.BusinessCheckName,
			Since:     pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: business-health query failed, rule skipped", "rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return businessHealthReason(stats, rule.BusinessCheckName, rule.Comparison, rule.ThresholdValue, rule.WindowMinutes)
	case "connector_error_rate":
		stats, err := s.q.GetConnectorErrorRateStats(ctx, generated.GetConnectorErrorRateStatsParams{
			ProjectID:    rule.ProjectID,
			OrgID:        rule.OrgID,
			SourceKind:   rule.ConnectorSourceKind,
			Provider:     rule.ConnectorProvider,
			Operation:    rule.ConnectorOperation,
			Channel:      rule.ConnectorChannel,
			ErrorCode:    rule.ConnectorErrorCode,
			ConnectionID: rule.ConnectorConnectionID,
			CompletedAt:  pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: connector error-rate query failed, rule skipped",
				"rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return connectorErrorRateReason(stats, rule.ConnectorProvider, rule.ConnectorOperation, rule.ConnectorChannel, rule.ConnectorErrorCode, rule.Comparison, rule.ThresholdValue, rule.WindowMinutes, rule.ConnectorConnectionID)
	case "connector_quota":
		stats, err := s.q.GetConnectorQuotaStats(ctx, generated.GetConnectorQuotaStatsParams{
			Metric:       rule.ConnectorQuotaMetric,
			ProjectID:    rule.ProjectID,
			OrgID:        rule.OrgID,
			SourceKind:   rule.ConnectorSourceKind,
			Provider:     rule.ConnectorProvider,
			Operation:    rule.ConnectorOperation,
			Channel:      rule.ConnectorChannel,
			ConnectionID: rule.ConnectorConnectionID,
			CompletedAt:  pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: connector quota query failed, rule skipped", "rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return connectorQuotaReason(stats, rule.ConnectorProvider, rule.ConnectorOperation, rule.ConnectorChannel, rule.ConnectorQuotaMetric, rule.Comparison, rule.ThresholdValue, rule.WindowMinutes, rule.ConnectorConnectionID)
	case "connector_freshness":
		lastCompletedAt, err := s.q.GetConnectorFreshnessStats(ctx, generated.GetConnectorFreshnessStatsParams{
			ProjectID:    rule.ProjectID,
			OrgID:        rule.OrgID,
			SourceKind:   rule.ConnectorSourceKind,
			Provider:     rule.ConnectorProvider,
			Operation:    rule.ConnectorOperation,
			Channel:      rule.ConnectorChannel,
			ConnectionID: rule.ConnectorConnectionID,
			BaselineAt:   rule.ConnectorFreshnessBaselineAt,
		})
		if err != nil {
			slog.Error("watchdog: connector freshness query failed, rule skipped", "rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return connectorFreshnessReason(lastCompletedAt, rule.ConnectorProvider, rule.ConnectorOperation, rule.ConnectorChannel, rule.ConnectorFreshnessBaselineAt, rule.CreatedAt, now, rule.WindowMinutes, rule.ConnectorConnectionID)
	case "connector_sync_health":
		stats, err := s.q.GetConnectorSyncHealthStats(ctx, generated.GetConnectorSyncHealthStatsParams{
			OrgID:        rule.OrgID,
			ConnectionID: rule.ConnectorConnectionID,
			Provider:     rule.ConnectorProvider,
			Operation:    rule.ConnectorOperation,
			Cutoff:       pgtype.Timestamptz{Time: now.Add(-time.Duration(rule.WindowMinutes) * time.Minute), Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: connector sync health query failed, rule skipped", "rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return connectorSyncHealthReason(stats, rule.ConnectorProvider, rule.ConnectorOperation, rule.ConnectorConnectionID, rule.ConnectorSyncMode, rule.Threshold, rule.WindowMinutes)
	case "log_severity":
		stats, err := s.q.GetLogSeverityStats(ctx, generated.GetLogSeverityStatsParams{
			ProjectID: rule.ProjectID,
			OrgID:     rule.OrgID,
			Severity:  rule.LogSeverity,
			Since:     pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: log severity query failed, rule skipped",
				"rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return logSeverityReason(stats, rule.LogSeverity, rule.Threshold, rule.WindowMinutes)
	case "release_error_rate":
		stats, err := s.q.GetReleaseErrorRateStats(ctx, generated.GetReleaseErrorRateStatsParams{
			ProjectID:      rule.ProjectID,
			OrgID:          rule.OrgID,
			ReleaseVersion: rule.ReleaseVersion,
			Since:          pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: release error-rate query failed, rule skipped",
				"rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return releaseErrorRateReason(stats, rule.ReleaseVersion, rule.Comparison, rule.ThresholdValue, rule.WindowMinutes)
	case "release_freshness":
		rows, err := s.q.GetReleaseFreshnessStats(ctx, generated.GetReleaseFreshnessStatsParams{
			ProjectID:      rule.ProjectID,
			OrgID:          rule.OrgID,
			ReleaseVersion: rule.ReleaseVersion,
		})
		if err != nil {
			slog.Error("watchdog: release freshness query failed, rule skipped",
				"rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		if len(rows) == 0 {
			return ""
		}
		return releaseFreshnessReason(rows[0], rule.ReleaseVersion, rule.WindowMinutes)
	case "service_freshness":
		lastHeartbeatAt, err := s.q.GetServiceFreshnessStats(ctx, generated.GetServiceFreshnessStatsParams{
			ProjectID:      rule.ProjectID,
			OrgID:          rule.OrgID,
			ServiceName:    rule.ServiceName,
			ReleaseVersion: rule.ReleaseVersion,
			BaselineAt:     rule.ServiceFreshnessBaselineAt,
		})
		if err != nil {
			slog.Error("watchdog: service freshness query failed, rule skipped", "rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return serviceFreshnessReason(lastHeartbeatAt, rule.ServiceName, rule.ReleaseVersion, rule.ServiceFreshnessBaselineAt, rule.CreatedAt, now, rule.WindowMinutes)
	case "usage_threshold":
		stats, err := s.q.GetUsageThresholdStats(ctx, generated.GetUsageThresholdStatsParams{
			ProjectID: rule.ProjectID,
			OrgID:     rule.OrgID,
			Surface:   rule.UsageSurface,
			Operation: rule.UsageOperation,
			Since:     pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: usage threshold query failed, rule skipped",
				"rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return usageThresholdReason(stats, rule.UsageSurface, rule.UsageOperation, rule.UsageMeasure, rule.Comparison, rule.Threshold, rule.WindowMinutes)
	case "deploy_failure":
		failures, err := s.q.GetDeployFailureStats(ctx, generated.GetDeployFailureStatsParams{
			ProjectID: rule.ProjectID,
			OrgID:     rule.OrgID,
			Since:     pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: deploy failure query failed, rule skipped",
				"rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return deployFailureReason(failures, rule.Threshold, rule.WindowMinutes)
	case "deploy_acceptance":
		stats, err := s.q.GetDeployAcceptanceStats(ctx, generated.GetDeployAcceptanceStatsParams{
			ProjectID:   rule.ProjectID,
			OrgID:       rule.OrgID,
			Since:       pgtype.Timestamptz{Time: dayStart, Valid: true},
			GraceCutoff: pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: deploy acceptance query failed, rule skipped",
				"rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return deployAcceptanceReason(stats, rule.Threshold, rule.WindowMinutes)
	case "rollback":
		rollbacks, err := s.q.GetRollbackStats(ctx, generated.GetRollbackStatsParams{
			ProjectID: rule.ProjectID,
			OrgID:     rule.OrgID,
			Since:     pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: rollback query failed, rule skipped", "rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return rollbackReason(rollbacks, rule.Threshold, rule.WindowMinutes)
	case "api_key_hygiene":
		nowTS := pgtype.Timestamptz{Time: now, Valid: true}
		horizon := time.Duration(rule.WindowMinutes) * time.Minute
		keyCount, err := s.q.GetAPIKeyHygieneStats(ctx, generated.GetAPIKeyHygieneStatsParams{
			OrgID:         rule.OrgID,
			Mode:          rule.ApiKeyHygieneMode,
			OlderThan:     pgtype.Timestamptz{Time: now.Add(-horizon), Valid: true},
			AsOf:          nowTS,
			ExpiresBefore: pgtype.Timestamptz{Time: now.Add(horizon), Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: api key hygiene query failed, rule skipped", "rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return apiKeyHygieneReason(keyCount, rule.ApiKeyHygieneMode, rule.Threshold, rule.WindowMinutes)
	case "ai_triage":
		stats, err := s.q.GetAITriageStats(ctx, generated.GetAITriageStatsParams{
			ProjectID: rule.ProjectID,
			OrgID:     rule.OrgID,
			Cutoff:    pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: ai triage query failed, rule skipped", "rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return aiTriageReason(stats, rule.AiTriageMode, rule.Threshold, rule.WindowMinutes)
	case "ingest_acceptance":
		stats, err := s.q.GetIngestAcceptanceStats(ctx, generated.GetIngestAcceptanceStatsParams{
			ProjectID: rule.ProjectID,
			OrgID:     rule.OrgID,
			Cutoff:    pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			slog.Error("watchdog: ingest acceptance query failed, rule skipped", "rule", rule.ID, "type", rule.Type, "project", rule.ProjectID, "error", err)
			return ""
		}
		return ingestAcceptanceReason(stats, rule.Threshold, rule.WindowMinutes)
	}
	return ""
}

// serviceFreshnessReason evaluates one exact service/release heartbeat. The
// query already applies the tenant/project predicate and exact message match;
// this function only decides whether the bounded grace period has elapsed.
// A missing series remains quiet during the initial grace period after rule
// creation/baseline, so configuring a rule does not page immediately.
func serviceFreshnessReason(lastHeartbeatAt pgtype.Timestamptz, service, release string, baseline, createdAt pgtype.Timestamptz, now time.Time, windowMin int32) string {
	if service == "" || release == "" || windowMin < 1 || windowMin > 720 {
		return ""
	}
	start := createdAt.Time
	if baseline.Valid {
		start = baseline.Time
	}
	if start.IsZero() {
		return ""
	}
	age := now.Sub(start)
	if lastHeartbeatAt.Valid {
		// A heartbeat exactly at the baseline is still a real heartbeat; it is
		// stale only once its age is strictly beyond the configured grace.
		if !lastHeartbeatAt.Time.Before(start) {
			age = now.Sub(lastHeartbeatAt.Time)
		}
	}
	if age < time.Duration(windowMin)*time.Minute {
		return ""
	}
	if !lastHeartbeatAt.Valid || lastHeartbeatAt.Time.Before(start) {
		return fmt.Sprintf("Service %s (release %s) has sent no heartbeat for %dm after baseline (age %.0fm)", service, release, windowMin, age.Minutes())
	}
	return fmt.Sprintf("Service %s (release %s) heartbeat is stale by %dm (age %.0fm)", service, release, windowMin, age.Minutes())
}

// usageThresholdReason evaluates a fixed usage surface/operation against the
// aggregate counters in one bounded window. API-key and MCP rows are org-wide;
// the SQL query has already applied that explicit branch while retaining the
// owning project's org boundary. A missing series never fires.
func usageThresholdReason(stats *generated.GetUsageThresholdStatsRow, surface, operation, measure, comparison string, threshold, windowMin int32) string {
	if stats == nil || stats.BucketCount < 1 || threshold < 1 || windowMin < 1 || windowMin > 720 {
		return ""
	}
	value := stats.RequestCount
	switch measure {
	case "errors":
		value = stats.ErrorCount
	case "denied":
		value = stats.DeniedCount
	case "requests":
	default:
		return ""
	}
	if value < 0 {
		return ""
	}
	fired := false
	switch comparison {
	case "gt":
		fired = value > int64(threshold)
	case "gte":
		fired = value >= int64(threshold)
	case "lt":
		fired = value < int64(threshold)
	case "lte":
		fired = value <= int64(threshold)
	default:
		return ""
	}
	if !fired {
		return ""
	}
	return fmt.Sprintf("Usage %s/%s %s reached %d in %dm (observed %d)", surface, operation, measure, threshold, windowMin, value)
}

// logSeverityReason evaluates one bounded canonical log-level count. A
// missing series never fires, so creating a rule before a service emits that
// severity stays quiet. Only aggregate counts enter the alert body.
func logSeverityReason(logCount int64, severity string, threshold, windowMin int32) string {
	if logCount < 1 || severity == "" || threshold < 1 || threshold > 1_000_000 || windowMin < 1 || windowMin > 720 {
		return ""
	}
	if logCount < int64(threshold) {
		return ""
	}
	return fmt.Sprintf("Log severity %s reached %d in %dm (observed %d records)", severity, threshold, windowMin, logCount)
}

// deployFailureReason evaluates only failed or blocked deploy-stage receipts.
// A missing series never fires, and the count is already scoped by the SQL
// query to one project and organization. Rollbacks use their own explicit
// receipt stage and are intentionally excluded here.
func deployFailureReason(failures int64, threshold, windowMin int32) string {
	if failures < 1 || threshold < 1 || threshold > 1_000_000 || windowMin < 1 || windowMin > 720 {
		return ""
	}
	if failures < int64(threshold) {
		return ""
	}
	return fmt.Sprintf("Deploy stage failed or was blocked %d time(s) in %dm (threshold %d)", failures, windowMin, threshold)
}

// deployAcceptanceReason pages only when a passed deploy has aged beyond its
// grace period without a matching passed synthetic receipt. The query already
// enforces tenant, project, issue, and target-revision identity; this function
// exposes only the aggregate count and bounded grace period.
func deployAcceptanceReason(acceptanceCount int64, threshold, graceMin int32) string {
	if acceptanceCount < 1 || threshold < 1 || threshold > 1_000_000 || graceMin < 1 || graceMin > 720 {
		return ""
	}
	if acceptanceCount < int64(threshold) {
		return ""
	}
	return fmt.Sprintf("Passed deploys without synthetic verification: %d older than %dm (threshold %d)", acceptanceCount, graceMin, threshold)
}

// rollbackReason evaluates only explicit terminal rollback receipts. Ordinary
// deploy failures and deploy successes never satisfy this rule.
func rollbackReason(rollbacks int64, threshold, windowMin int32) string {
	if rollbacks < 1 || threshold < 1 || threshold > 1_000_000 || windowMin < 1 || windowMin > 720 {
		return ""
	}
	if rollbacks < int64(threshold) {
		return ""
	}
	return fmt.Sprintf("Explicit rollback recorded %d time(s) in %dm (threshold %d)", rollbacks, windowMin, threshold)
}

// apiKeyHygieneReason evaluates only the aggregate count from the tenant-scoped
// API-key query. A zero count never fires, and the mode is fixed by the API and
// database check constraint so a rule cannot turn into a credential listing.
func apiKeyHygieneReason(keyCount int64, mode string, threshold, horizonMin int32) string {
	if keyCount < 1 || threshold < 1 || threshold > 1_000_000 || horizonMin < 1 || horizonMin > maxAPIKeyHygieneWindowMinutes {
		return ""
	}
	if mode != "unused" && mode != "expiring" {
		return ""
	}
	if keyCount < int64(threshold) {
		return ""
	}
	if mode == "unused" {
		return fmt.Sprintf("API-key hygiene: %d inactive/unused key(s) older than %dm (threshold %d)", keyCount, horizonMin, threshold)
	}
	return fmt.Sprintf("API-key hygiene: %d key(s) expiring within %dm (threshold %d)", keyCount, horizonMin, threshold)
}

// aiTriageReason evaluates payload-free aggregate receipt counts. Failed
// receipts are counted inside the rolling window; stuck receipts are still in
// the started state, have no completion timestamp, and are older than that
// same window. The reason intentionally contains only counts and fixed mode
// labels, never issue text, prompts, completions, or provider error details.
func aiTriageReason(stats *generated.GetAITriageStatsRow, mode string, threshold, windowMin int32) string {
	if stats == nil || threshold < 1 || threshold > 1_000_000 || windowMin < 1 || windowMin > 720 {
		return ""
	}
	var count int64
	switch mode {
	case "failed":
		count = stats.FailedCount
		if count < int64(threshold) {
			return ""
		}
		return fmt.Sprintf("AI triage failed %d time(s) in %dm (threshold %d; %d with safe error codes)", count, windowMin, threshold, stats.FailedErrorCount)
	case "stuck":
		count = stats.StuckCount
		if count < int64(threshold) {
			return ""
		}
		return fmt.Sprintf("AI triage has %d stuck receipt(s) older than %dm (threshold %d)", count, windowMin, threshold)
	default:
		return ""
	}
}

// ingestAcceptanceReason pages when a request-level acceptance receipt has
// stayed in_progress past the configured deadline. A stale row means Flare
// cannot prove that the sender's 2xx response was durably completed, usually
// because the process died after creating the receipt. The SQL query has
// already applied the project and organization fence and returns only a count.
func ingestAcceptanceReason(staleCount int64, threshold, windowMin int32) string {
	if staleCount < 1 || threshold < 1 || threshold > 1_000_000 || windowMin < 1 || windowMin > 720 {
		return ""
	}
	if staleCount < int64(threshold) {
		return ""
	}
	return fmt.Sprintf("Ingest acceptance has %d incomplete receipt(s) older than %dm (threshold %d)", staleCount, windowMin, threshold)
}

// releaseErrorRateReason evaluates one aggregate release series. A missing
// release or an empty release series never fires; this keeps a rule created
// before the next deploy quiet until that release has actually reported data.
// The query chooses the latest recent release when ReleaseVersion is empty and
// always scopes the explicit selector to the same tenant and project.
func releaseErrorRateReason(stats *generated.GetReleaseErrorRateStatsRow, selectedVersion, comparison string, threshold float64, windowMin int32) string {
	if stats == nil || stats.EventCount < 1 || stats.ErrorCount < 0 || stats.ErrorCount > stats.EventCount || stats.ReleaseVersion == "" || windowMin < 1 || math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 || threshold > 1 {
		return ""
	}
	if selectedVersion != "" && selectedVersion != stats.ReleaseVersion {
		return ""
	}
	rate := float64(stats.ErrorCount) / float64(stats.EventCount)
	fired := false
	switch comparison {
	case "gt":
		fired = rate > threshold
	case "gte":
		fired = rate >= threshold
	case "lt":
		fired = rate < threshold
	case "lte":
		fired = rate <= threshold
	default:
		return ""
	}
	if !fired {
		return ""
	}
	return fmt.Sprintf("Release %s error rate %s %.4g in %dm (observed %.2f across %d events)", stats.ReleaseVersion, comparison, threshold, windowMin, rate, stats.EventCount)
}

// releaseFreshnessReason evaluates only a release with an explicit deploy
// marker. Age is measured from the last canonical service-up heartbeat, or
// from the deploy marker before the first heartbeat. A missing row never
// fires, so an arbitrary historical event release cannot page.
func releaseFreshnessReason(stats *generated.GetReleaseFreshnessStatsRow, selectedVersion string, windowMin int32) string {
	if stats == nil || selectedVersion == "" || stats.Version != selectedVersion || windowMin < 1 || windowMin > 720 {
		return ""
	}
	if !stats.DeployMarkedAt.Valid || stats.AgeSeconds < 0 || math.IsNaN(stats.AgeSeconds) || math.IsInf(stats.AgeSeconds, 0) {
		return ""
	}
	threshold := float64(windowMin * 60)
	if stats.AgeSeconds < threshold {
		return ""
	}
	if !stats.LastHeartbeatAt.Valid {
		return fmt.Sprintf("Release %s has sent no service heartbeat for %dm after deploy (age %.0fm)", stats.Version, windowMin, stats.AgeSeconds/60)
	}
	return fmt.Sprintf("Release %s service heartbeat is stale by %dm (age %.0fm)", stats.Version, windowMin, stats.AgeSeconds/60)
}

// tracePercentileReason evaluates one bounded span operation. A missing span
// series never fires, so creating a rule before the first deployment does not
// page. The percentile is computed by PostgreSQL and only the aggregate row is
// considered here; span attributes and raw traces never enter the alert body.
func tracePercentileReason(stats *generated.GetTracePercentileStatsRow, spanName string, percentile int32, comparison string, threshold float64, windowMin int32) string {
	if stats == nil || stats.Points < 1 || spanName == "" || windowMin < 1 || percentile < 1 || percentile > 99 || math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		return ""
	}
	var observed float64
	switch comparison {
	case "gt", "gte":
		observed = stats.PercentileValue
	case "lt", "lte":
		observed = stats.PercentileValue
	default:
		return ""
	}
	fired := false
	switch comparison {
	case "gt":
		fired = observed > threshold
	case "gte":
		fired = observed >= threshold
	case "lt":
		fired = observed < threshold
	case "lte":
		fired = observed <= threshold
	}
	if !fired {
		return ""
	}
	return fmt.Sprintf("Trace latency: %s p%d %.1fms %s %.1fms over %dm (%d spans)", spanName, percentile, observed, comparison, threshold, windowMin, stats.Points)
}

// serviceLevelReason evaluates observed availability for one exact operation
// against the configured target.  A missing series stays quiet because no
// received span cannot establish either success or failure; pair this rule
// with service_freshness/check-in monitoring when producer silence matters.
func serviceLevelReason(stats *generated.GetServiceLevelStatsRow, spanName string, latencyMS int32, target float64, windowMin int32) string {
	if stats == nil || stats.SpanCount < 1 || stats.GoodCount < 0 || stats.GoodCount > stats.SpanCount || spanName == "" || latencyMS < 1 || windowMin < 1 || windowMin > 720 || math.IsNaN(target) || math.IsInf(target, 0) || target <= 0 || target >= 1 {
		return ""
	}
	availability := float64(stats.GoodCount) / float64(stats.SpanCount)
	if availability >= target {
		return ""
	}
	return fmt.Sprintf("Service-level SLO: %s availability %.5f below target %.5f (good %d/%d within %.0fms over %dm)", spanName, availability, target, stats.GoodCount, stats.SpanCount, float64(latencyMS), windowMin)
}

// connectorErrorRateReason evaluates one bounded, project-scoped connector
// series. Empty operation/channel/error-code selectors are represented in the
// reason as "*" and a missing series never fires so a newly-created rule does
// not page before its connector has attempted any work.
func connectorErrorRateReason(stats *generated.GetConnectorErrorRateStatsRow, provider, operation, channel, errorCode, comparison string, threshold float64, windowMin int32, connectionIDs ...string) string {
	if stats == nil || stats.Attempts < 1 || provider == "" || windowMin < 1 || math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 || threshold > 1 {
		return ""
	}
	if stats.ErrorCount < 0 || stats.ErrorCount > stats.Attempts {
		return ""
	}
	rate := float64(stats.ErrorCount) / float64(stats.Attempts)
	triggered := false
	switch comparison {
	case "gt":
		triggered = rate > threshold
	case "gte":
		triggered = rate >= threshold
	case "lt":
		triggered = rate < threshold
	case "lte":
		triggered = rate <= threshold
	default:
		return ""
	}
	if !triggered {
		return ""
	}
	if operation == "" {
		operation = "*"
	}
	if channel == "" {
		channel = "*"
	}
	if errorCode == "" {
		errorCode = "*"
	}
	return fmt.Sprintf("Connector %s/%s/%s/%s%s error rate %s %.4g in %dm (observed %.2f across %d attempts)",
		provider, operation, channel, errorCode, connectorConnectionLabel(connectionIDs...), comparison, threshold, windowMin, rate, stats.Attempts)
}

// connectorQuotaReason evaluates the latest provider-reported quota, retry-after,
// or sync-lag hint in a bounded project-scoped series. A missing series never
// fires, and only the latest integer observation is used so stale values do
// not create a false all-clear after a newer rate-limit response.
func connectorQuotaReason(stats *generated.GetConnectorQuotaStatsRow, provider, operation, channel, metric, comparison string, threshold float64, windowMin int32, connectionIDs ...string) string {
	if stats == nil || stats.Points < 1 || provider == "" || windowMin < 1 || math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 {
		return ""
	}
	if metric != "quota_remaining" && metric != "retry_after_ms" && metric != "sync_lag_ms" {
		return ""
	}
	max := float64(maxConnectorQuotaRemaining)
	if metric == "retry_after_ms" {
		max = float64(maxConnectorRetryAfterMS)
	} else if metric == "sync_lag_ms" {
		max = float64(maxConnectorSyncLagMS)
	}
	if threshold > max || stats.LatestValue < 0 || float64(stats.LatestValue) > max {
		return ""
	}
	observed := float64(stats.LatestValue)
	triggered := false
	switch comparison {
	case "gt":
		triggered = observed > threshold
	case "gte":
		triggered = observed >= threshold
	case "lt":
		triggered = observed < threshold
	case "lte":
		triggered = observed <= threshold
	default:
		return ""
	}
	if !triggered {
		return ""
	}
	if operation == "" {
		operation = "*"
	}
	if channel == "" {
		channel = "*"
	}
	label := "quota remaining"
	unit := ""
	if metric == "retry_after_ms" {
		label = "retry-after"
		unit = "ms"
	} else if metric == "sync_lag_ms" {
		label = "sync lag"
		unit = "ms"
	}
	return fmt.Sprintf("Connector %s/%s/%s%s %s %s %.4g%s in %dm (latest %.4g%s across %d observations)",
		provider, operation, channel, connectorConnectionLabel(connectionIDs...), label, comparison, threshold, unit, windowMin, observed, unit, stats.Points)
}

// connectorFreshnessReason evaluates the age of the latest completed attempt
// for one explicitly configured connector series. The query only considers
// attempts after the durable rule baseline; when there is no matching row,
// the baseline/creation time is the observation point. This keeps an empty
// series quiet until its configured cadence elapses and never infers silence
// from an unconfigured or arbitrary historical series.
func connectorFreshnessReason(lastCompletedAt pgtype.Timestamptz, provider, operation, channel string, baseline, createdAt pgtype.Timestamptz, now time.Time, windowMin int32, connectionIDs ...string) string {
	if provider == "" || windowMin < 1 || windowMin > 720 {
		return ""
	}
	if operation == "" {
		operation = "*"
	}
	if channel == "" {
		channel = "*"
	}
	start := baseline
	if !start.Valid {
		start = createdAt
	}
	if !start.Valid {
		return ""
	}
	observed := start.Time
	if lastCompletedAt.Valid && lastCompletedAt.Time.After(observed) {
		observed = lastCompletedAt.Time
	}
	if now.Sub(observed) < time.Duration(windowMin)*time.Minute {
		return ""
	}
	series := fmt.Sprintf("%s/%s/%s%s", provider, operation, channel, connectorConnectionLabel(connectionIDs...))
	if !lastCompletedAt.Valid {
		return fmt.Sprintf("Connector %s has no completed attempt for %dm (since freshness baseline)", series, windowMin)
	}
	return fmt.Sprintf("Connector %s has no completed attempt for %dm (last completed %s ago)", series, windowMin, durationAge(now.Sub(observed)))
}

// connectorSyncHealthReason evaluates payload-free durable sync-state counts.
// The SQL query already fences by organization, exact registered connection,
// exact operation, and excludes intentionally paused/revoked installations.
func connectorSyncHealthReason(stats *generated.GetConnectorSyncHealthStatsRow, provider, operation, connectionID, mode string, threshold, windowMin int32) string {
	if stats == nil || provider == "" || operation == "" || connectionID == "" || threshold < 1 || threshold > 1_000_000 || windowMin < 1 || windowMin > 720 {
		return ""
	}
	var count int64
	label := ""
	switch mode {
	case "failed":
		count, label = stats.FailedCount, "failed sync state(s)"
	case "overdue":
		count, label = stats.OverdueCount, "overdue sync state(s) or lease(s)"
	default:
		return ""
	}
	if count < int64(threshold) {
		return ""
	}
	return fmt.Sprintf("Connector sync %s/%s/connection:%s has %d %s in %dm (threshold %d)", provider, operation, connectionID, count, label, windowMin, threshold)
}

// connectorConnectionLabel keeps installation identity explicit in watchdog
// reasons without exposing provider payloads or credentials. Empty selectors
// intentionally remain provider-wide and preserve the legacy reason format.
func connectorConnectionLabel(connectionIDs ...string) string {
	if len(connectionIDs) == 0 || connectionIDs[0] == "" {
		return ""
	}
	return "/connection:" + connectionIDs[0]
}

func durationAge(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	minutes := int64(age / time.Minute)
	if minutes < 1 {
		return "<1m"
	}
	return fmt.Sprintf("%dm", minutes)
}

// countEventsSince counts EVERY event (heartbeats included). Silence only.
func (s *Server) countEventsSince(ctx context.Context, projectID, org string, since time.Time) (int64, error) {
	return s.q.CountEventsForProjectSince(ctx, generated.CountEventsForProjectSinceParams{
		ProjectID:  projectID,
		OrgID:      org,
		ReceivedAt: pgtype.Timestamptz{Time: since, Valid: true},
	})
}

// countActionableEventsSince excludes info/debug (heartbeats, breadcrumbs).
// Anomaly only.
func (s *Server) countActionableEventsSince(ctx context.Context, projectID, org string, since time.Time) (int64, error) {
	return s.q.CountActionableEventsForProjectSince(ctx, generated.CountActionableEventsForProjectSinceParams{
		ProjectID:  projectID,
		OrgID:      org,
		ReceivedAt: pgtype.Timestamptz{Time: since, Valid: true},
	})
}

// anomalyReason fires when the recent window's event count both exceeds the
// absolute floor AND is at least `multiplier` times the project's own trailing
// baseline rate (average per same-sized window over the last 24h, excluding the
// recent window). multiplier is the rule threshold; a missing/<2 multiplier
// defaults to 3.
func anomalyReason(recent, day24h int64, windowMin, multiplier int32) string {
	if windowMin < 1 || recent < anomalyMinEvents {
		return ""
	}
	if multiplier < 2 {
		multiplier = 3
	}
	// day24h is a strict superset of recent; clamp defensively so a skew can
	// never produce a negative baseline (and a nonsensical alert message).
	if recent > day24h {
		day24h = recent
	}
	windowsPerDay := (24 * 60) / int64(windowMin)
	if windowsPerDay < 2 {
		return "" // window too coarse (>12h) to have a baseline
	}
	baseline := float64(day24h-recent) / float64(windowsPerDay-1)
	threshold := float64(multiplier) * baseline
	if threshold < anomalyMinEvents {
		threshold = anomalyMinEvents
	}
	if float64(recent) < threshold {
		return ""
	}
	if baseline <= 0 {
		// Near-silent baseline: the multiplier is undefined, so report the
		// burst plainly rather than presenting the raw count as an "Nx".
		return fmt.Sprintf("Anomaly: %d events in %dm from a near-silent baseline", recent, windowMin)
	}
	return fmt.Sprintf("Anomaly: %d events in %dm, ~%.0f/window baseline (%.1fx)",
		recent, windowMin, baseline, float64(recent)/baseline)
}

// silenceReason fires when a normally-active project (>= minActive events over
// the last 24h) has gone completely quiet in the recent window. minActive is
// the rule threshold; a missing/<1 threshold defaults to 20.
func silenceReason(recent, day24h int64, minActive int32) string {
	if recent != 0 {
		return ""
	}
	if minActive < 1 {
		minActive = 20
	}
	if day24h < int64(minActive) {
		return "" // not normally active; silence is expected
	}
	return fmt.Sprintf("Silence: no events recently, but ~%d in the last 24h", day24h)
}

// metricThresholdReason evaluates extrema over one bounded metric window.
// High-water comparisons use max; low-water comparisons use min. A missing
// series never fires, which prevents a newly-created rule from paging before
// the producer has emitted its first point.
func metricThresholdReason(stats *generated.GetMetricThresholdStatsRow, metricName, comparison string, threshold float64, windowMin int32) string {
	if stats == nil || stats.Points < 1 || metricName == "" || windowMin < 1 || math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		return ""
	}
	var observed float64
	switch comparison {
	case "gt", "gte":
		observed = stats.MaxValue
	case "lt", "lte":
		observed = stats.MinValue
	default:
		return ""
	}
	triggered := false
	switch comparison {
	case "gt":
		triggered = observed > threshold
	case "gte":
		triggered = observed >= threshold
	case "lt":
		triggered = observed < threshold
	case "lte":
		triggered = observed <= threshold
	}
	if !triggered {
		return ""
	}
	return fmt.Sprintf("Metric %q %s %.4g in %dm (observed %.4g across %d points)",
		metricName, comparison, threshold, windowMin, observed, stats.Points)
}

func maxInt32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
