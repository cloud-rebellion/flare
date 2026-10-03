package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/metrics"
)

const (
	usageWriteTimeout  = 750 * time.Millisecond
	defaultUsageHours  = 24
	maxUsageHours      = 720
	defaultUsageLimit  = 100
	maxUsageLimit      = 100
	maxUsageIdentifier = 128
)

// usageDelta is the only data that enters durable usage accounting. All
// dimensions are canonicalized below; in particular, no request path,
// credential value, tenant label, user identifier, or provider payload is
// persisted.
type usageDelta struct {
	OrgID        string
	ProjectID    string
	CredentialID string
	Surface      string
	Operation    string
	Requests     int64
	Errors       int64
	Denied       int64
}

func (s *Server) recordUsage(ctx context.Context, d usageDelta) {
	if s == nil || s.q == nil || d.OrgID == "" {
		return
	}
	d.Surface = canonicalUsageSurface(d.Surface)
	d.Operation = canonicalUsageOperation(d.Surface, d.Operation)
	d.ProjectID = boundedUsageIdentifier(d.ProjectID)
	d.CredentialID = boundedUsageIdentifier(d.CredentialID)
	if d.Requests < 0 || d.Errors < 0 || d.Denied < 0 || d.Requests == 0 && d.Errors == 0 && d.Denied == 0 {
		return
	}
	// A one-minute bucket bounds row growth while preserving a durable,
	// queryable count. Detach from a client disconnect but keep the write short
	// and non-fatal: usage accounting must never turn a telemetry response into
	// a retry storm.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageWriteTimeout)
	defer cancel()
	err := s.q.UpsertUsageBucket(writeCtx, generated.UpsertUsageBucketParams{
		OrgID:        d.OrgID,
		ProjectID:    d.ProjectID,
		CredentialID: d.CredentialID,
		Surface:      d.Surface,
		Operation:    d.Operation,
		BucketStart:  pgtype.Timestamptz{Time: time.Now().UTC().Truncate(time.Minute), Valid: true},
		RequestCount: d.Requests,
		ErrorCount:   d.Errors,
		DeniedCount:  d.Denied,
	})
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	metrics.Inc("flare_usage_bucket_writes", map[string]string{
		"surface": d.Surface, "operation": d.Operation, "outcome": outcome,
	})
	if err != nil {
		// Do not expose SQL details or request data to the caller. The private
		// metrics counter above and the server log give operators a bounded
		// signal that usage persistence is unavailable.
		slog.Error("record usage bucket", "error", err)
	}
}

func boundedUsageIdentifier(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > maxUsageIdentifier {
		return ""
	}
	return v
}

func canonicalUsageSurface(surface string) string {
	switch surface {
	case "dsn", "api_key", "mcp":
		return surface
	default:
		return "other"
	}
}

func canonicalUsageOperation(surface, operation string) string {
	operation = strings.TrimSpace(operation)
	switch surface {
	case "dsn":
		switch operation {
		case "envelope", "event", "logs", "metrics", "traces", "connector_attempts", "checkins", "symbols", "otlp_logs", "otlp_traces", "otlp_metrics":
			return operation
		}
	case "api_key":
		switch operation {
		case "read", "write", "mcp", "other":
			return operation
		}
	case "mcp":
		switch operation {
		case "initialize", "ping", "tools_list", "tools_call", "tool_overview", "tool_list_projects", "tool_list_issues", "tool_get_issue", "tool_search_logs", "tool_get_trace", "tool_query_metrics", "tool_update_issue_status", "tool_triage_issue", "other":
			return operation
		}
	}
	return "other"
}

func dsnUsageOperation(r *http.Request) string {
	if r == nil {
		return "other"
	}
	path := r.URL.Path
	switch {
	case strings.Contains(path, "/envelope"):
		return "envelope"
	case strings.HasSuffix(path, "/events"), strings.HasSuffix(path, "/store"):
		return "event"
	case strings.HasSuffix(path, "/logs"):
		if path == "/otlp/v1/logs" {
			return "otlp_logs"
		}
		return "logs"
	case strings.HasSuffix(path, "/traces"):
		return "otlp_traces"
	case strings.HasSuffix(path, "/metrics"):
		if path == "/otlp/v1/metrics" {
			return "otlp_metrics"
		}
		return "metrics"
	case strings.HasSuffix(path, "/connector-attempts"):
		return "connector_attempts"
	case strings.Contains(path, "/checkins/"):
		return "checkins"
	case strings.HasSuffix(path, "/symbol-artifacts"):
		return "symbols"
	default:
		return "other"
	}
}

func apiKeyUsageOperation(r *http.Request) string {
	if r == nil {
		return "other"
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/mcp") {
		return "mcp"
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return "read"
	}
	if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
		return "write"
	}
	return "other"
}

func mcpUsageOperation(req mcpRequest) string {
	switch req.Method {
	case "initialize":
		return "initialize"
	case "ping":
		return "ping"
	case "tools/list":
		return "tools_list"
	case "tools/call":
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(req.Params, &p); err == nil {
			switch p.Name {
			case "overview", "list_projects", "list_issues", "get_issue", "search_logs", "get_trace", "query_metrics", "update_issue_status", "triage_issue":
				return "tool_" + p.Name
			}
		}
		return "tools_call"
	default:
		return "other"
	}
}

type usageResponse struct {
	ProjectID    string     `json:"project_id,omitempty"`
	CredentialID string     `json:"credential_id,omitempty"`
	Surface      string     `json:"surface"`
	Operation    string     `json:"operation"`
	Requests     int64      `json:"requests"`
	Errors       int64      `json:"errors"`
	Denied       int64      `json:"denied"`
	ErrorRate    float64    `json:"error_rate"`
	FirstSeenAt  *time.Time `json:"first_seen_at,omitempty"`
	LastSeenAt   *time.Time `json:"last_seen_at,omitempty"`
}

func (s *Server) handleListUsage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListUsageBucketsByOrg(r.Context(), generated.ListUsageBucketsByOrgParams{
		OrgID:       orgIDFrom(r.Context()),
		BucketStart: pgtype.Timestamptz{Time: time.Now().UTC().Add(-time.Duration(usageHours(r)) * time.Hour), Valid: true},
		Limit:       int32(usageLimit(r)),
	})
	if err != nil {
		slogError(w, "list usage buckets", err)
		return
	}
	out := make([]usageResponse, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		v := usageResponse{
			ProjectID: row.ProjectID, CredentialID: row.CredentialID,
			Surface: row.Surface, Operation: row.Operation,
			Requests: row.RequestCount, Errors: row.ErrorCount, Denied: row.DeniedCount,
			FirstSeenAt: receiptTimestamp(row.FirstSeenAt), LastSeenAt: receiptTimestamp(row.LastSeenAt),
		}
		if v.Requests > 0 {
			v.ErrorRate = float64(v.Errors) / float64(v.Requests)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func usageHours(r *http.Request) int {
	v := parseBoundedInt(r, "hours", defaultUsageHours, maxUsageHours)
	return v
}

func usageLimit(r *http.Request) int {
	return parseBoundedInt(r, "limit", defaultUsageLimit, maxUsageLimit)
}

func parseBoundedInt(r *http.Request, name string, fallback, maximum int) int {
	if r == nil {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(name)))
	if err != nil || n <= 0 {
		return fallback
	}
	if n > maximum {
		return maximum
	}
	return n
}
