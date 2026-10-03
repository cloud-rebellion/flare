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
	defaultConnectorHealthHours = 24
	maxConnectorHealthHours     = 720
	defaultConnectorHealthLimit = 100
	maxConnectorHealthLimit     = 100
)

// connectorHealthResponse is an aggregate projection. It contains no tenant
// identifiers, URLs, provider response text, credentials, or payloads.
type connectorHealthResponse struct {
	// Empty means the receipt came from a legacy/external DSN bridge without a
	// registered installation. Non-empty IDs are opaque tenant-owned registry
	// identifiers and let operators separate two installs of one provider.
	ConnectionID    string     `json:"connection_id,omitempty"`
	Provider        string     `json:"provider"`
	Operation       string     `json:"operation"`
	Channel         string     `json:"channel"`
	Attempts        int64      `json:"attempts"`
	OKCount         int64      `json:"ok_count"`
	ErrorCount      int64      `json:"error_count"`
	ErrorRate       float64    `json:"error_rate"`
	AvgElapsedMs    float64    `json:"avg_elapsed_ms"`
	MaxElapsedMs    int64      `json:"max_elapsed_ms"`
	LastAttemptedAt *time.Time `json:"last_attempted_at,omitempty"`
	LastCompletedAt *time.Time `json:"last_completed_at,omitempty"`
}

// handleListConnectorHealth returns org-scoped outbound connector health.
// Provider calls are org-level by default; project attribution, when present,
// is retained in the receipt for future project filtering but is not needed to
// authorize this bounded org aggregate.
func (s *Server) handleListConnectorHealth(w http.ResponseWriter, r *http.Request) {
	since := time.Now().UTC().Add(-time.Duration(connectorHealthHours(r)) * time.Hour)
	rows, err := s.q.ListConnectorHealthByOrg(r.Context(), generated.ListConnectorHealthByOrgParams{
		OrgID:       orgIDFrom(r.Context()),
		CompletedAt: pgtype.Timestamptz{Time: since, Valid: true},
		Limit:       int32(connectorHealthLimit(r)),
	})
	if err != nil {
		slogError(w, "list connector health", err)
		return
	}
	out := make([]connectorHealthResponse, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		out = append(out, toConnectorHealthResponse(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleListProjectConnectorHealth is the project-scoped counterpart to the
// org aggregate. Internal connectors may be org-owned, while DSN bridges and
// app integrations carry a project id; this route keeps that provenance
// visible without widening the tenant boundary.
func (s *Server) handleListProjectConnectorHealth(w http.ResponseWriter, r *http.Request) {
	project, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{
		ID: chi.URLParam(r, "id"), OrgScope: orgIDFrom(r.Context()),
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	since := time.Now().UTC().Add(-time.Duration(connectorHealthHours(r)) * time.Hour)
	rows, err := s.q.ListConnectorHealthByProject(r.Context(), generated.ListConnectorHealthByProjectParams{
		ProjectID:   pgtype.Text{String: project.ID, Valid: true},
		OrgID:       project.OrgID,
		CompletedAt: pgtype.Timestamptz{Time: since, Valid: true},
		Limit:       int32(connectorHealthLimit(r)),
	})
	if err != nil {
		slogError(w, "list project connector health", err)
		return
	}
	out := make([]connectorHealthResponse, 0, len(rows))
	for _, row := range rows {
		if row != nil {
			mapped := connectorHealthResponse{
				ConnectionID:    row.ConnectionID,
				Provider:        row.Provider,
				Operation:       row.Operation,
				Channel:         row.Channel,
				Attempts:        row.Attempts,
				OKCount:         row.OkCount,
				ErrorCount:      row.ErrorCount,
				AvgElapsedMs:    row.AvgElapsedMs,
				MaxElapsedMs:    row.MaxElapsedMs,
				LastAttemptedAt: receiptTimestamp(row.LastAttemptedAt),
				LastCompletedAt: receiptTimestamp(row.LastCompletedAt),
			}
			if mapped.Attempts > 0 {
				mapped.ErrorRate = float64(mapped.ErrorCount) / float64(mapped.Attempts)
			}
			out = append(out, mapped)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func connectorHealthHours(r *http.Request) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("hours")))
	if err != nil || v <= 0 {
		return defaultConnectorHealthHours
	}
	if v > maxConnectorHealthHours {
		return maxConnectorHealthHours
	}
	return v
}

func connectorHealthLimit(r *http.Request) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	if err != nil || v <= 0 {
		return defaultConnectorHealthLimit
	}
	if v > maxConnectorHealthLimit {
		return maxConnectorHealthLimit
	}
	return v
}

func toConnectorHealthResponse(row *generated.ListConnectorHealthByOrgRow) connectorHealthResponse {
	out := connectorHealthResponse{
		ConnectionID:    row.ConnectionID,
		Provider:        row.Provider,
		Operation:       row.Operation,
		Channel:         row.Channel,
		Attempts:        row.Attempts,
		OKCount:         row.OkCount,
		ErrorCount:      row.ErrorCount,
		AvgElapsedMs:    row.AvgElapsedMs,
		MaxElapsedMs:    row.MaxElapsedMs,
		LastAttemptedAt: receiptTimestamp(row.LastAttemptedAt),
		LastCompletedAt: receiptTimestamp(row.LastCompletedAt),
	}
	if row.Attempts > 0 {
		out.ErrorRate = float64(row.ErrorCount) / float64(row.Attempts)
	}
	return out
}
