package api

import (
	"net/http"
	"time"

	"github.com/bright-interaction/flare/internal/db/generated"
)

// analyticsExportHealthResponse is a global, payload-free cold-tier
// projection. It contains no tenant IDs, object paths, provider endpoints,
// credentials, or error text. bytes_known is false when DuckDB httpfs cannot
// report a remote S3/MinIO object length.
type analyticsExportHealthResponse struct {
	Configured       bool   `json:"configured"`
	ColdTier         string `json:"cold_tier"`
	ConnectionStatus string `json:"connection_status"`
	// RetentionStatus describes only the configured retention/export policy and
	// the latest durable export outcome. It never infers that a partition was
	// dropped or that an object-store quota is available.
	RetentionStatus string `json:"retention_status"`
	// QuotaStatus is explicit because this service has no provider quota API.
	// Local mode has no configured quota, while S3/MinIO quota remains a
	// provider concern. Neither value is derived from exported_bytes.
	QuotaStatus       string     `json:"quota_status"`
	SuccessCount      int64      `json:"success_count"`
	FailureCount      int64      `json:"failure_count"`
	AttemptedCount    int64      `json:"attempted_count"`
	ExportedBytes     int64      `json:"exported_bytes"`
	BytesKnown        bool       `json:"bytes_known"`
	BytesObservations int64      `json:"bytes_observations"`
	AvgLatencyMS      float64    `json:"avg_latency_ms"`
	MaxLatencyMS      int64      `json:"max_latency_ms"`
	LatestAttemptAt   *time.Time `json:"latest_attempt_at,omitempty"`
	LatestSuccessAt   *time.Time `json:"latest_success_at,omitempty"`
	LatestFailureAt   *time.Time `json:"latest_failure_at,omitempty"`
	LatestErrorCode   string     `json:"latest_error_code,omitempty"`
}

// handleAnalyticsHealth returns aggregate cold-tier exporter telemetry. The
// route is authenticated (viewer+) because it is an operator API, while the
// same bounded projection is also included in /readyz for probes.
func (s *Server) handleAnalyticsHealth(w http.ResponseWriter, r *http.Request) {
	if s.q == nil {
		writeErr(w, http.StatusServiceUnavailable, "analytics health unavailable")
		return
	}
	row, err := s.q.GetExportHealthStats(r.Context())
	if err != nil {
		slogError(w, "get analytics export health", err)
		return
	}
	writeJSON(w, http.StatusOK, toAnalyticsExportHealthResponse(s, row))
}

func toAnalyticsExportHealthResponse(s *Server, row *generated.GetExportHealthStatsRow) analyticsExportHealthResponse {
	bytesKnown := row.BytesObservations > 0 && s.cfg.S3Endpoint == ""
	exportedBytes := row.ExportedBytes
	if !bytesKnown {
		exportedBytes = 0
	}
	out := analyticsExportHealthResponse{
		Configured:        s.analytics != nil,
		ColdTier:          analyticsColdTier(s.cfg.S3Endpoint != ""),
		ConnectionStatus:  exportConnectionStatus(row.LatestStatus),
		RetentionStatus:   retentionExportStatus(s, row.LatestStatus),
		QuotaStatus:       objectStoreQuotaStatus(s),
		SuccessCount:      row.SuccessCount,
		FailureCount:      row.FailureCount,
		AttemptedCount:    row.AttemptedCount,
		ExportedBytes:     exportedBytes,
		BytesKnown:        bytesKnown,
		BytesObservations: row.BytesObservations,
		AvgLatencyMS:      row.AvgLatencyMs,
		MaxLatencyMS:      row.MaxLatencyMs,
		LatestAttemptAt:   receiptTimestamp(row.LatestAttemptAt),
		LatestSuccessAt:   receiptTimestamp(row.LatestSuccessAt),
		LatestFailureAt:   receiptTimestamp(row.LatestFailureAt),
		LatestErrorCode:   row.LatestErrorCode,
	}
	if !out.Configured {
		out.ConnectionStatus = "disabled"
	}
	return out
}

// retentionExportStatus is deliberately an observed-state projection. A
// successful export row proves only that one export completed; it does not
// claim that every aged partition has been archived. Likewise, a failed row
// does not infer data loss or a rollback. The partition worker remains the
// authority for export-before-drop enforcement.
func retentionExportStatus(s *Server, status string) string {
	if s.cfg.AllowDropWithoutExport {
		return "drop_without_export"
	}
	if s.analytics == nil {
		return "paused"
	}
	switch status {
	case "succeeded":
		return "observed_success"
	case "failed":
		return "observed_failure"
	case "attempted":
		return "in_progress"
	default:
		return "unknown"
	}
}

// objectStoreQuotaStatus makes the missing provider capability explicit. The
// local backend has no configured quota setting, and S3-compatible backends do
// not expose quota usage through DuckDB's current COPY path. Keep this bounded
// and payload-free instead of treating ledger bytes as current occupancy.
func objectStoreQuotaStatus(s *Server) string {
	if s.cfg.S3Endpoint != "" {
		return "provider_unknown"
	}
	return "not_configured"
}

func analyticsColdTier(s3 bool) string {
	if s3 {
		return "s3"
	}
	return "local"
}

func exportConnectionStatus(status string) string {
	switch status {
	case "succeeded":
		return "observed_success"
	case "failed":
		return "observed_failure"
	case "attempted":
		return "in_progress"
	default:
		return "unknown"
	}
}
