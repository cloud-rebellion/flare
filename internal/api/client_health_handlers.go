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
	defaultClientHealthHours = 24
	maxClientHealthHours     = 720 // 30 days, keeping the query on hot data.
	defaultClientHealthLimit = 100
	maxClientHealthLimit     = 100
)

// clientHealthResponse is an aggregate projection of client/runtime dimensions.
// It deliberately excludes event bodies, stacktraces, tenant identifiers, and
// raw labels so browser and mobile health can be shared with read-only users.
type clientHealthResponse struct {
	Platform           string     `json:"platform"`
	Environment        string     `json:"environment"`
	Release            string     `json:"release"`
	EventCount         int64      `json:"event_count"`
	ErrorCount         int64      `json:"error_count"`
	ErrorRate          float64    `json:"error_rate"`
	AffectedIssueCount int64      `json:"affected_issue_count"`
	FirstSeen          *time.Time `json:"first_seen,omitempty"`
	LastSeen           *time.Time `json:"last_seen,omitempty"`
}

// handleListClientHealth returns bounded client/runtime health for one project.
// The query carries both the authenticated organization and route project so a
// project id cannot become a cross-tenant aggregate oracle.
func (s *Server) handleListClientHealth(w http.ResponseWriter, r *http.Request) {
	hours := clientHealthHours(r)
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	rows, err := s.q.ListClientHealthByProject(r.Context(), generated.ListClientHealthByProjectParams{
		ProjectID: chi.URLParam(r, "id"),
		OrgID:     orgIDFrom(r.Context()),
		Since:     pgtype.Timestamptz{Time: since, Valid: true},
		RowLimit:  int32(clientHealthLimit(r)),
	})
	if err != nil {
		slogError(w, "list client health", err)
		return
	}
	out := make([]clientHealthResponse, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		out = append(out, toClientHealthResponse(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func clientHealthHours(r *http.Request) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("hours")))
	if err != nil || v <= 0 {
		return defaultClientHealthHours
	}
	if v > maxClientHealthHours {
		return maxClientHealthHours
	}
	return v
}

func clientHealthLimit(r *http.Request) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	if err != nil || v <= 0 {
		return defaultClientHealthLimit
	}
	if v > maxClientHealthLimit {
		return maxClientHealthLimit
	}
	return v
}

func toClientHealthResponse(row *generated.ListClientHealthByProjectRow) clientHealthResponse {
	out := clientHealthResponse{
		Platform:           row.Platform,
		Environment:        row.Environment,
		Release:            row.Release,
		EventCount:         row.EventCount,
		ErrorCount:         row.ErrorCount,
		AffectedIssueCount: row.AffectedIssueCount,
		FirstSeen:          receiptTimestamp(row.FirstSeen),
		LastSeen:           receiptTimestamp(row.LastSeen),
	}
	if row.EventCount > 0 {
		out.ErrorRate = float64(row.ErrorCount) / float64(row.EventCount)
	}
	return out
}
