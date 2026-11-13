package analytics

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// HourBucket is one (hour, severity) log-volume cell.
type HourBucket struct {
	Hour     time.Time `json:"hour"`
	Severity string    `json:"severity"`
	Count    int64     `json:"count"`
}

// SpanLatency is per-operation latency percentiles over the window.
type SpanLatency struct {
	Name  string  `json:"name"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
	Count int64   `json:"count"`
}

// ServiceLevel is a bounded SLO aggregate over sampled spans. Good spans have
// a non-error status and complete within the configured latency objective.
// ErrorBudgetConsumed is the observed bad-event rate divided by the allowed
// error rate (1 - target); values above 1 mean the budget is exhausted.
type ServiceLevel struct {
	Operation           string  `json:"operation,omitempty"`
	WindowHours         int     `json:"window_hours"`
	LatencyThresholdMs  float64 `json:"latency_threshold_ms"`
	Target              float64 `json:"target"`
	SpanCount           int64   `json:"span_count"`
	GoodCount           int64   `json:"good_count"`
	ErrorCount          int64   `json:"error_count"`
	SlowCount           int64   `json:"slow_count"`
	ErrorRate           float64 `json:"error_rate"`
	Availability        float64 `json:"availability"`
	ErrorBudgetConsumed float64 `json:"error_budget_consumed"`
}

// source builds a "(hot UNION ALL cold) AS src" subquery exposing exactly the
// requested columns. The cold (Parquet) arm is included only when cold data is
// known to exist, because DuckDB errors on a read_parquet glob that matches
// nothing. Tenant scoping is applied by the OUTER query against src, covering
// both arms.
func (m *Manager) source(table string, cols []string) string {
	colList := strings.Join(cols, ", ")
	hot := fmt.Sprintf("SELECT %s FROM hot.public.%s", colList, table)
	if !m.coldAvailable(table) {
		return "(" + hot + ") AS src"
	}
	glob := m.cfg.parquetRoot() + "/" + table + "/dt=*/part.parquet"
	cold := fmt.Sprintf("SELECT %s FROM read_parquet('%s', union_by_name => true)", colList, escapeSQL(glob))
	return "(" + hot + " UNION ALL " + cold + ") AS src"
}

// coldAvailable reports whether any exported Parquet exists for the table.
func (m *Manager) coldAvailable(table string) bool {
	if m.cfg.S3Endpoint == "" {
		return archiveHasDay(m.cfg.ParquetDir, table)
	}
	// S3: gate on the export_log marker (best-effort; missing table => no cold).
	// row_count > 0 is required: an empty aged partition writes a row_count=0
	// marker WITHOUT producing any Parquet file, so counting markers alone made
	// this claim cold data existed and every analytics query then failed on a
	// read_parquet glob that matches nothing.
	var n int
	if err := m.db.QueryRow(`SELECT count(*) FROM hot.public.export_log WHERE tbl = ? AND row_count > 0`, table).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// LogVolumeByHour returns hourly log counts by severity for a project over the
// last sinceHours hours, querying hot Postgres unioned with cold Parquet.
func (m *Manager) LogVolumeByHour(ctx context.Context, projectID, orgID string, sinceHours int) ([]HourBucket, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, fmt.Errorf("analytics: closed")
	}
	src := m.source("logs", []string{"observed_at", "severity", "project_id", "org_id"})
	q := fmt.Sprintf(`
		SELECT date_trunc('hour', observed_at) AS hour, severity, count(*) AS cnt
		FROM %s
		WHERE project_id = ? AND org_id = ? AND observed_at >= now() - (? * INTERVAL '1 hour')
		GROUP BY 1, 2
		ORDER BY 1`, src)
	rows, err := m.db.QueryContext(ctx, q, projectID, orgID, sinceHours)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HourBucket{}
	for rows.Next() {
		var b HourBucket
		if err := rows.Scan(&b.Hour, &b.Severity, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SpanLatencyPercentiles returns p50/p95/p99 duration per span name for a
// project over the last sinceHours hours. This is the kind of GROUP BY +
// approx_quantile aggregation DuckDB does far faster than the PG planner.
func (m *Manager) SpanLatencyPercentiles(ctx context.Context, projectID, orgID string, sinceHours int) ([]SpanLatency, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, fmt.Errorf("analytics: closed")
	}
	src := m.source("spans", []string{"name", "duration_ms", "project_id", "org_id", "start_time"})
	q := fmt.Sprintf(`
		SELECT name,
		       approx_quantile(duration_ms, 0.50) AS p50,
		       approx_quantile(duration_ms, 0.95) AS p95,
		       approx_quantile(duration_ms, 0.99) AS p99,
		       count(*) AS cnt
		FROM %s
		WHERE project_id = ? AND org_id = ? AND start_time >= now() - (? * INTERVAL '1 hour')
		GROUP BY 1
		ORDER BY p95 DESC
		LIMIT 50`, src)
	rows, err := m.db.QueryContext(ctx, q, projectID, orgID, sinceHours)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SpanLatency{}
	for rows.Next() {
		var s SpanLatency
		if err := rows.Scan(&s.Name, &s.P50, &s.P95, &s.P99, &s.Count); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SpanServiceLevel returns one tenant-scoped availability/error-budget
// aggregate for spans in the requested window. The operation filter is
// optional; when present it is an exact operation name. A span is good only
// when its status is not an error and its duration is within latencyThreshold.
// This is evidence over received/sampled spans, not a claim about requests
// that emitted no trace.
func (m *Manager) SpanServiceLevel(ctx context.Context, projectID, orgID string, sinceHours int, operation string, latencyThreshold, target float64) (*ServiceLevel, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, fmt.Errorf("analytics: closed")
	}
	src := m.source("spans", []string{"name", "status", "duration_ms", "project_id", "org_id", "start_time"})
	whereOperation := ""
	if operation != "" {
		whereOperation = " AND name = ?"
	}
	// The latency threshold is deliberately repeated in the CASE expression so
	// the aggregate remains one row and no raw span data crosses the API.
	q := fmt.Sprintf(`
		SELECT count(*)::BIGINT AS total,
		       COALESCE(sum(CASE WHEN status NOT ILIKE '%%error%%' AND duration_ms <= ? THEN 1 ELSE 0 END), 0)::BIGINT AS good,
		       COALESCE(sum(CASE WHEN status ILIKE '%%error%%' THEN 1 ELSE 0 END), 0)::BIGINT AS errors,
		       COALESCE(sum(CASE WHEN status NOT ILIKE '%%error%%' AND duration_ms > ? THEN 1 ELSE 0 END), 0)::BIGINT AS slow
		FROM %s
		WHERE project_id = ? AND org_id = ?
		  AND start_time >= now() - (? * INTERVAL '1 hour')%s`, src, whereOperation)
	// DuckDB binds each '?' in positional order. The SELECT has two threshold
	// placeholders, followed by the tenant/window predicates and optional name.
	args := []any{latencyThreshold, latencyThreshold, projectID, orgID, sinceHours}
	if operation != "" {
		args = append(args, operation)
	}
	var total, good, errors, slow int64
	if err := m.db.QueryRowContext(ctx, q, args...).Scan(&total, &good, &errors, &slow); err != nil {
		return nil, err
	}
	out := &ServiceLevel{
		Operation:          operation,
		WindowHours:        sinceHours,
		LatencyThresholdMs: latencyThreshold,
		Target:             target,
		SpanCount:          total,
		GoodCount:          good,
		ErrorCount:         errors,
		SlowCount:          slow,
	}
	if total > 0 {
		out.Availability = float64(good) / float64(total)
		out.ErrorRate = 1 - out.Availability
	}
	allowedErrorRate := 1 - target
	if allowedErrorRate > 0 {
		out.ErrorBudgetConsumed = out.ErrorRate / allowedErrorRate
	}
	return out, nil
}
