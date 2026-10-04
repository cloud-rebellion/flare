package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/bright-interaction/flare/internal/connectorsync"
	"github.com/bright-interaction/flare/internal/hubspot"
	"github.com/bright-interaction/flare/internal/id"
)

// connectorSyncRunResponse is a bounded execution receipt. It contains the
// durable state projection only; provider response bodies, credentials, cursor
// bytes, worker IDs, and organization IDs never cross this API boundary.
type connectorSyncRunResponse struct {
	Operation     string     `json:"operation"`
	State         string     `json:"state"`
	Outcome       string     `json:"outcome,omitempty"`
	StatusClass   string     `json:"status_class,omitempty"`
	ErrorCode     string     `json:"error_code,omitempty"`
	ReceiptStatus string     `json:"receipt_status,omitempty"`
	Claimed       bool       `json:"claimed"`
	Skipped       bool       `json:"skipped"`
	SkipReason    string     `json:"skip_reason,omitempty"`
	NextRunAt     *time.Time `json:"next_run_at,omitempty"`
}

// handleRunConnectorSync executes one bounded HubSpot health sync. It is an
// explicit admin action and scheduler hook; Flare does not create a ticker or
// poll every tenant automatically. The lease in connectorsync prevents two
// callers from running the same installation concurrently.
func (s *Server) handleRunConnectorSync(w http.ResponseWriter, r *http.Request) {
	orgID := orgIDFrom(r.Context())
	connectionID := chi.URLParam(r, "id")
	worker := s.hubspotSync
	if worker == nil {
		probe := s.hubspotProbe
		if probe == nil {
			probe = hubspot.New()
		}
		worker = connectorsync.NewHubSpotWorker(s.q, s.secrets, probe)
	}
	result, err := worker.Run(r.Context(), orgID, connectionID, "manual:"+id.New())
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "connector connection not found")
		return
	}
	if err != nil {
		slogError(w, "run connector sync", err)
		writeErr(w, http.StatusServiceUnavailable, "connector sync is unavailable")
		return
	}
	if result.Skipped {
		switch result.SkipReason {
		case "unsupported_provider":
			writeErr(w, http.StatusUnprocessableEntity, "sync is not supported for this provider")
			return
		case "connection_disabled":
			writeErr(w, http.StatusConflict, "connector connection is not enabled for sync")
			return
		case "credential_missing":
			writeErr(w, http.StatusBadRequest, "connector connection has no credential")
			return
		}
	}
	if result.Claimed {
		s.audit(r.Context(), "connector.sync", connectionID)
	}
	status := http.StatusOK
	if result.Skipped && result.SkipReason == "lease_busy" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, connectorSyncRunResponse{
		Operation: connectorsync.HubSpotAccountDetailsOperation,
		State:     result.State.Status, Outcome: result.Outcome, StatusClass: result.StatusClass,
		ErrorCode: result.ErrorCode, ReceiptStatus: result.ReceiptStatus, Claimed: result.Claimed, Skipped: result.Skipped,
		SkipReason: result.SkipReason, NextRunAt: receiptTimestamp(result.State.NextRunAt),
	})
}
