package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
)

const (
	defaultConnectorQuotaHealthHours = 24
	maxConnectorQuotaHealthHours     = 720
	defaultConnectorQuotaHealthLimit = 100
	maxConnectorQuotaHealthLimit     = 100
)

// connectorQuotaHealthResponse is a metadata-only projection. Nullable values
// remain omitted when a provider did not report that hint in the window.
type connectorQuotaHealthResponse struct {
	SourceKind string `json:"source_kind"`
	// Empty means a legacy/external receipt without a registered installation.
	// Registered IDs are opaque and tenant-owned; no provider payload is exposed.
	ConnectionID      string     `json:"connection_id,omitempty"`
	Provider          string     `json:"provider"`
	Operation         string     `json:"operation"`
	Channel           string     `json:"channel"`
	QuotaPoints       int64      `json:"quota_points"`
	RetryPoints       int64      `json:"retry_points"`
	SyncLagPoints     int64      `json:"sync_lag_points"`
	QuotaRemaining    *int64     `json:"quota_remaining,omitempty"`
	QuotaObservedAt   *time.Time `json:"quota_observed_at,omitempty"`
	RetryAfterMS      *int64     `json:"retry_after_ms,omitempty"`
	RetryObservedAt   *time.Time `json:"retry_observed_at,omitempty"`
	SyncLagMS         *int64     `json:"sync_lag_ms,omitempty"`
	SyncLagObservedAt *time.Time `json:"sync_lag_observed_at,omitempty"`
	LastCompletedAt   *time.Time `json:"last_completed_at,omitempty"`
}

// handleListConnectorQuotaHealth returns org-scoped latest provider quota and
// rate-limit and sync-lag hints. The result contains bounded integers and timestamps only;
// no provider response payload or credential is retained or returned.
func (s *Server) handleListConnectorQuotaHealth(w http.ResponseWriter, r *http.Request) {
	source, ok := connectorSourceKind(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "source_kind must be server_observed, client_reported or legacy_unknown")
		return
	}
	since := time.Now().UTC().Add(-time.Duration(connectorQuotaHealthHours(r)) * time.Hour)
	rows, err := s.q.ListConnectorQuotaHealthByOrg(r.Context(), generated.ListConnectorQuotaHealthByOrgParams{
		OrgID:       orgIDFrom(r.Context()),
		CompletedAt: pgtype.Timestamptz{Time: since, Valid: true},
		SourceKind:  source,
		Limit:       int32(connectorQuotaHealthLimit(r)),
	})
	if err != nil {
		slogError(w, "list connector quota health", err)
		return
	}
	out := make([]connectorQuotaHealthResponse, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		out = append(out, toConnectorQuotaHealthResponse(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleListProjectConnectorQuotaHealth returns the same bounded quota and
// synchronization projection for one project. Resolve the project through the
// caller's organization before querying attempts so a guessed project id cannot
// turn this into a cross-tenant read.
func (s *Server) handleListProjectConnectorQuotaHealth(w http.ResponseWriter, r *http.Request) {
	source, ok := connectorSourceKind(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "source_kind must be server_observed, client_reported or legacy_unknown")
		return
	}
	project, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{
		ID: chi.URLParam(r, "id"), OrgScope: orgIDFrom(r.Context()),
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	since := time.Now().UTC().Add(-time.Duration(connectorQuotaHealthHours(r)) * time.Hour)
	rows, err := s.q.ListConnectorQuotaHealthByProject(r.Context(), generated.ListConnectorQuotaHealthByProjectParams{
		ProjectID:   pgtype.Text{String: project.ID, Valid: true},
		OrgID:       project.OrgID,
		CompletedAt: pgtype.Timestamptz{Time: since, Valid: true},
		SourceKind:  source,
		Limit:       int32(connectorQuotaHealthLimit(r)),
	})
	if err != nil {
		slogError(w, "list project connector quota health", err)
		return
	}
	out := make([]connectorQuotaHealthResponse, 0, len(rows))
	for _, row := range rows {
		if row != nil {
			out = append(out, toProjectConnectorQuotaHealthResponse(row))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func connectorQuotaHealthHours(r *http.Request) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("hours")))
	if err != nil || v <= 0 {
		return defaultConnectorQuotaHealthHours
	}
	if v > maxConnectorQuotaHealthHours {
		return maxConnectorQuotaHealthHours
	}
	return v
}

func connectorQuotaHealthLimit(r *http.Request) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	if err != nil || v <= 0 {
		return defaultConnectorQuotaHealthLimit
	}
	if v > maxConnectorQuotaHealthLimit {
		return maxConnectorQuotaHealthLimit
	}
	return v
}

func toConnectorQuotaHealthResponse(row *generated.ListConnectorQuotaHealthByOrgRow) connectorQuotaHealthResponse {
	return connectorQuotaHealthResponseFromValues(
		row.SourceKind, row.ConnectionID, row.Provider, row.Operation, row.Channel,
		row.QuotaPoints, row.RetryPoints, row.SyncLagPoints,
		row.QuotaRemaining, row.QuotaObservedAt,
		row.RetryAfterMs, row.RetryObservedAt,
		row.SyncLagMs, row.SyncLagObservedAt, row.LastCompletedAt,
	)
}

func toProjectConnectorQuotaHealthResponse(row *generated.ListConnectorQuotaHealthByProjectRow) connectorQuotaHealthResponse {
	return connectorQuotaHealthResponseFromValues(
		row.SourceKind, row.ConnectionID, row.Provider, row.Operation, row.Channel,
		row.QuotaPoints, row.RetryPoints, row.SyncLagPoints,
		row.QuotaRemaining, row.QuotaObservedAt,
		row.RetryAfterMs, row.RetryObservedAt,
		row.SyncLagMs, row.SyncLagObservedAt, row.LastCompletedAt,
	)
}

func connectorQuotaHealthResponseFromValues(sourceKind, connectionID, provider, operation, channel string, quotaPoints, retryPoints, syncLagPoints int64, quotaRemaining pgtype.Int8, quotaObservedAt pgtype.Timestamptz, retryAfterMS pgtype.Int8, retryObservedAt pgtype.Timestamptz, syncLagMS pgtype.Int8, syncLagObservedAt pgtype.Timestamptz, lastCompletedAt pgtype.Timestamptz) connectorQuotaHealthResponse {
	out := connectorQuotaHealthResponse{
		SourceKind:        sourceKind,
		ConnectionID:      connectionID,
		Provider:          provider,
		Operation:         operation,
		Channel:           channel,
		QuotaPoints:       quotaPoints,
		RetryPoints:       retryPoints,
		SyncLagPoints:     syncLagPoints,
		QuotaObservedAt:   receiptTimestamp(quotaObservedAt),
		RetryObservedAt:   receiptTimestamp(retryObservedAt),
		SyncLagObservedAt: receiptTimestamp(syncLagObservedAt),
		LastCompletedAt:   receiptTimestamp(lastCompletedAt),
	}
	if quotaRemaining.Valid {
		v := quotaRemaining.Int64
		out.QuotaRemaining = &v
	}
	if retryAfterMS.Valid {
		v := retryAfterMS.Int64
		out.RetryAfterMS = &v
	}
	if syncLagMS.Valid {
		v := syncLagMS.Int64
		out.SyncLagMS = &v
	}
	return out
}
