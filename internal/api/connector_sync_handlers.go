package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultConnectorSyncHealthLimit = 100
	maxConnectorSyncHealthLimit     = 100
)

// connectorSyncHealthResponse is a bounded control-plane projection. It
// exposes lease and cursor presence/version metadata so operators can see
// whether a worker is making progress, while omitting cursor material,
// credentials, provider payloads, and tenant identifiers.
type connectorSyncHealthResponse struct {
	ID               string     `json:"id"`
	ConnectionID     string     `json:"connection_id"`
	Provider         string     `json:"provider"`
	InstallationName string     `json:"installation_name,omitempty"`
	ConnectionStatus string     `json:"connection_status"`
	Operation        string     `json:"operation"`
	State            string     `json:"state"`
	CursorConfigured bool       `json:"cursor_configured"`
	CursorVersion    int64      `json:"cursor_version"`
	AttemptCount     int64      `json:"attempt_count"`
	LastStartedAt    *time.Time `json:"last_started_at,omitempty"`
	LastCompletedAt  *time.Time `json:"last_completed_at,omitempty"`
	LastSuccessAt    *time.Time `json:"last_success_at,omitempty"`
	LastErrorCode    string     `json:"last_error_code,omitempty"`
	LastSyncLagMS    *int64     `json:"last_sync_lag_ms,omitempty"`
	NextRunAt        *time.Time `json:"next_run_at,omitempty"`
	LeaseActive      bool       `json:"lease_active"`
	LeaseExpiresAt   *time.Time `json:"lease_expires_at,omitempty"`
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
}

// handleListConnectorSyncHealth returns durable worker state for this tenant.
// It is intentionally a read-only projection; workers use the owner-guarded
// SQL claim/complete operations and never accept cursor or lease mutations from
// a browser/API client.
func (s *Server) handleListConnectorSyncHealth(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListConnectorSyncHealthByOrg(r.Context(), generated.ListConnectorSyncHealthByOrgParams{
		OrgID: orgIDFrom(r.Context()),
		Limit: int32(connectorSyncHealthLimit(r)),
	})
	if err != nil {
		slogError(w, "list connector sync health", err)
		return
	}
	out := make([]connectorSyncHealthResponse, 0, len(rows))
	for _, row := range rows {
		if row != nil {
			out = append(out, toConnectorSyncHealthResponse(row))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func connectorSyncHealthLimit(r *http.Request) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	if err != nil || v <= 0 {
		return defaultConnectorSyncHealthLimit
	}
	if v > maxConnectorSyncHealthLimit {
		return maxConnectorSyncHealthLimit
	}
	return v
}

func toConnectorSyncHealthResponse(row *generated.ListConnectorSyncHealthByOrgRow) connectorSyncHealthResponse {
	return connectorSyncHealthResponse{
		ID:               row.ID,
		ConnectionID:     row.ConnectionID,
		Provider:         row.Provider,
		InstallationName: row.InstallationName,
		ConnectionStatus: row.ConnectionStatus,
		Operation:        row.Operation,
		State:            row.State,
		CursorConfigured: row.CursorConfigured,
		CursorVersion:    row.CursorVersion,
		AttemptCount:     row.AttemptCount,
		LastStartedAt:    receiptTimestamp(row.LastStartedAt),
		LastCompletedAt:  receiptTimestamp(row.LastCompletedAt),
		LastSuccessAt:    receiptTimestamp(row.LastSuccessAt),
		LastErrorCode:    row.LastErrorCode,
		LastSyncLagMS:    nullableInt64(row.LastSyncLagMs),
		NextRunAt:        receiptTimestamp(row.NextRunAt),
		LeaseActive:      row.LeaseActive,
		LeaseExpiresAt:   receiptTimestamp(row.LeaseExpiresAt),
		UpdatedAt:        receiptTimestamp(row.UpdatedAt),
	}
}

func nullableInt64(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	out := v.Int64
	return &out
}
