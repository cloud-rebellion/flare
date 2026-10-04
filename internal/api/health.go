package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// healthWorkerSnapshot is deliberately small and payload-free. It describes
// the in-process bounded pools without exposing tenant or job information.
type healthWorkerSnapshot struct {
	Capacity  int `json:"capacity"`
	Busy      int `json:"busy"`
	Available int `json:"available"`
}

type healthDB struct {
	Healthy       bool  `json:"healthy"`
	LatencyMS     int64 `json:"latency_ms"`
	TotalConns    int32 `json:"total_conns"`
	AcquiredConns int32 `json:"acquired_conns"`
	IdleConns     int32 `json:"idle_conns"`
	MaxConns      int32 `json:"max_conns"`
}

type healthAnalytics struct {
	Configured bool                           `json:"configured"`
	Healthy    bool                           `json:"healthy"`
	Export     *analyticsExportHealthResponse `json:"export,omitempty"`
}

type healthRetention struct {
	Days               int    `json:"retention_days"`
	Policy             string `json:"policy"`
	ExportRequired     bool   `json:"export_required"`
	ColdTier           string `json:"cold_tier"`
	PartitionTelemetry string `json:"partition_telemetry"`
}

type healthQueue struct {
	Mode       string `json:"mode"`
	Queued     int    `json:"queued"`
	Durable    bool   `json:"durable"`
	DropOnFull bool   `json:"drop_on_full"`
}

// Aggregate outbox health contains no tenant identifiers or notice bodies.
type healthAutomationOutbox struct {
	State         string `json:"state"` // unconfigured | healthy | degraded | unavailable
	Pending       int64  `json:"pending"`
	Due           int64  `json:"due"`
	Leased        int64  `json:"leased"`
	Dead          int64  `json:"dead"`
	OldestSeconds int64  `json:"oldest_due_seconds"`
}

// Aggregate issue evaluation health contains no tenant or event details.
type healthIssueEvaluations struct {
	State                string `json:"state"` // healthy | degraded | unavailable
	Pending              int64  `json:"pending"`
	ExpiredLeases        int64  `json:"expired_leases"`
	Dead                 int64  `json:"dead"`
	OldestPendingSeconds int64  `json:"oldest_pending_seconds"`
	CountsCapped         bool   `json:"counts_capped"`
}

type healthResponse struct {
	Status            string                          `json:"status"`
	Release           string                          `json:"release"`
	SourceCommit      string                          `json:"source_commit"`
	DB                healthDB                        `json:"db"`
	Analytics         healthAnalytics                 `json:"analytics"`
	Retention         healthRetention                 `json:"retention"`
	ConnectorReceipts healthConnectorReceipts         `json:"connector_receipts"`
	Email             string                          `json:"email"`
	Workers           map[string]healthWorkerSnapshot `json:"workers"`
	Queue             healthQueue                     `json:"queue"`
	AutomationOutbox  healthAutomationOutbox          `json:"automation_outbox"`
	IssueEvaluations  healthIssueEvaluations          `json:"issue_evaluations"`
}

// workerSnapshot reports the current occupancy of a bounded pool. A nil
// channel is treated as an empty pool so health remains safe for lightweight
// test servers built without NewServer.
func workerSnapshot(ch chan struct{}) healthWorkerSnapshot {
	if ch == nil {
		return healthWorkerSnapshot{}
	}
	capacity := cap(ch)
	busy := len(ch)
	return healthWorkerSnapshot{Capacity: capacity, Busy: busy, Available: capacity - busy}
}

func retentionHealth(cfgAllowDrop bool, analyticsConfigured bool) (policy string, exportRequired bool) {
	if cfgAllowDrop {
		return "drop_without_export", false
	}
	if analyticsConfigured {
		return "export_before_drop", true
	}
	return "paused", true
}

func readinessStatus(dbOK, analyticsConfigured, analyticsOK bool, policy string) (status string, code int) {
	status, code = "ok", http.StatusOK
	if !dbOK {
		return "degraded", http.StatusServiceUnavailable
	}
	if (analyticsConfigured && !analyticsOK) || policy == "paused" {
		status = "degraded"
	}
	return status, code
}

func effectiveRetentionDays(days int) int {
	if days <= 0 {
		return 30
	}
	return days
}

// handleLive is intentionally independent of the database and providers. It
// answers the narrow question used by liveness probes: is the HTTP process
// serving requests?
func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady reports bounded process dependencies for load balancers and
// operators. Postgres is the only hard readiness dependency because it serves
// ingest and the hot telemetry path; optional analytics and retention state are
// surfaced as degraded while keeping HTTP 200 so a cold-tier outage does not
// eject a pod that can still receive data. No tenant data, provider payloads,
// credentials, or queue contents are returned.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	started := time.Now()
	db := healthDB{}
	if s.pool != nil {
		db.Healthy = s.pool.Ping(ctx) == nil
		db.LatencyMS = time.Since(started).Milliseconds()
		stat := s.pool.Stat()
		db.TotalConns = stat.TotalConns()
		db.AcquiredConns = stat.AcquiredConns()
		db.IdleConns = stat.IdleConns()
		db.MaxConns = stat.MaxConns()
	}

	analyticsConfigured := s.analytics != nil
	analyticsOK := false
	if analyticsConfigured {
		analyticsOK = s.analytics.Healthy(ctx)
	}
	var exportHealth *analyticsExportHealthResponse
	if s.q != nil {
		if row, err := s.q.GetExportHealthStats(ctx); err == nil {
			v := toAnalyticsExportHealthResponse(s, row)
			exportHealth = &v
		} else if analyticsConfigured {
			// A configured analytics layer whose durable export ledger cannot be
			// queried is degraded even when DuckDB's hot probe still succeeds.
			// Keep the optional dependency at HTTP 200, but do not claim `ok`.
			slog.Warn("analytics export health query failed", "error", err)
			analyticsOK = false
		}
	}
	policy, exportRequired := retentionHealth(s.cfg.AllowDropWithoutExport, analyticsConfigured)
	coldTier := "local"
	if s.cfg.S3Endpoint != "" {
		coldTier = "s3"
	}
	status, code := readinessStatus(db.Healthy, analyticsConfigured, analyticsOK, policy)
	queueHealth := healthAutomationOutbox{State: "unconfigured"}
	if s.automationOutbox != nil {
		if !db.Healthy {
			queueHealth.State = "unavailable"
		} else if stats, err := s.automationOutbox.Snapshot(ctx); err == nil {
			queueHealth = healthAutomationOutbox{
				State: "healthy", Pending: stats.Pending, Due: stats.Due,
				Leased: stats.Leased, Dead: stats.Dead,
			}
			if stats.Oldest != nil {
				queueHealth.OldestSeconds = max(0, int64(time.Since(*stats.Oldest).Seconds()))
			}
			if stats.Dead > 0 || stats.Due > 100 || queueHealth.OldestSeconds > 300 {
				queueHealth.State = "degraded"
				if status == "ok" {
					status = "degraded"
				}
			}
		} else {
			queueHealth.State = "unavailable"
			if status == "ok" {
				status = "degraded"
			}
			slog.Warn("automation outbox health query failed", "error", err)
		}
	}
	issueHealth := healthIssueEvaluations{State: "unavailable"}
	if db.Healthy {
		if snapshot, err := s.IssueEvaluationHealth(ctx); err == nil {
			issueHealth = healthIssueEvaluations{
				State: "healthy", Pending: snapshot.Pending,
				ExpiredLeases: snapshot.ExpiredLeases, Dead: snapshot.Dead,
				OldestPendingSeconds: snapshot.OldestPendingAgeSeconds,
				CountsCapped:         snapshot.CountsCapped,
			}
			if snapshot.Dead > 0 || snapshot.ExpiredLeases > 0 || snapshot.Pending > 500 || snapshot.OldestPendingAgeSeconds > 300 {
				issueHealth.State = "degraded"
				if status == "ok" {
					status = "degraded"
				}
			}
		} else {
			if status == "ok" {
				status = "degraded"
			}
			slog.Warn("issue evaluation health query failed", "error", err)
		}
	}

	workers := map[string]healthWorkerSnapshot{
		"default":     workerSnapshot(s.bgPool("default")),
		"auto_triage": workerSnapshot(s.bgPool("auto-triage")),
	}
	resp := healthResponse{
		Status:       status,
		Release:      s.cfg.Release,
		SourceCommit: s.cfg.SourceCommit,
		DB:           db,
		Analytics: healthAnalytics{
			Configured: analyticsConfigured,
			Healthy:    analyticsOK,
			Export:     exportHealth,
		},
		Retention: healthRetention{
			Days:               effectiveRetentionDays(s.cfg.RetentionDays),
			Policy:             policy,
			ExportRequired:     exportRequired,
			ColdTier:           coldTier,
			PartitionTelemetry: "log_only",
		},
		ConnectorReceipts: s.connectorReceiptHealthSnapshot(),
		Email: func() string {
			if s.cfg.EmailEnabled() {
				return "configured"
			}
			return "disabled"
		}(),
		Workers: workers,
		Queue: healthQueue{
			Mode:       "bounded_drop",
			Queued:     0,
			Durable:    false,
			DropOnFull: true,
		},
		AutomationOutbox: queueHealth,
		IssueEvaluations: issueHealth,
	}
	writeJSON(w, code, resp)
}

// handleHealth keeps the historical /api/health path as a compatibility alias
// for the richer unauthenticated readiness response.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.handleReady(w, r)
}
