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
	defaultReleaseHealthHours = 24
	maxReleaseHealthHours     = 720 // 30 days, keeping the query on hot data.
	defaultReleaseHealthLimit = 100
	maxReleaseHealthLimit     = 100
)

// releaseHealthResponse deliberately contains aggregate release health only.
// Event bodies, issue titles, and tenant identifiers stay behind their own
// authenticated, project-scoped endpoints.
type releaseHealthResponse struct {
	Version            string     `json:"version"`
	CreatedAt          *time.Time `json:"created_at,omitempty"`
	EventCount         int64      `json:"event_count"`
	ErrorCount         int64      `json:"error_count"`
	ErrorRate          float64    `json:"error_rate"`
	AffectedIssueCount int64      `json:"affected_issue_count"`
	FirstSeen          *time.Time `json:"first_seen,omitempty"`
	LastSeen           *time.Time `json:"last_seen,omitempty"`
}

// handleListReleaseHealth returns bounded release health for one project. The
// query carries both the authenticated org and the route project to prevent a
// project id from becoming a cross-tenant aggregate oracle.
func (s *Server) handleListReleaseHealth(w http.ResponseWriter, r *http.Request) {
	hours := releaseHealthHours(r)
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	rows, err := s.q.ListReleaseHealthByProject(r.Context(), generated.ListReleaseHealthByProjectParams{
		Since:     pgtype.Timestamptz{Time: since, Valid: true},
		ProjectID: chi.URLParam(r, "id"),
		OrgID:     orgIDFrom(r.Context()),
		RowLimit:  int32(releaseHealthLimit(r)),
	})
	if err != nil {
		slogError(w, "list release health", err)
		return
	}
	out := make([]releaseHealthResponse, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		out = append(out, toReleaseHealthResponse(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func releaseHealthHours(r *http.Request) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("hours")))
	if err != nil || v <= 0 {
		return defaultReleaseHealthHours
	}
	if v > maxReleaseHealthHours {
		return maxReleaseHealthHours
	}
	return v
}

func releaseHealthLimit(r *http.Request) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	if err != nil || v <= 0 {
		return defaultReleaseHealthLimit
	}
	if v > maxReleaseHealthLimit {
		return maxReleaseHealthLimit
	}
	return v
}

func toReleaseHealthResponse(row *generated.ListReleaseHealthByProjectRow) releaseHealthResponse {
	out := releaseHealthResponse{
		Version:            row.Version,
		EventCount:         row.EventCount,
		ErrorCount:         row.ErrorCount,
		AffectedIssueCount: row.AffectedIssueCount,
		CreatedAt:          receiptTimestamp(row.CreatedAt),
		FirstSeen:          receiptTimestamp(row.FirstSeen),
		LastSeen:           receiptTimestamp(row.LastSeen),
	}
	if row.EventCount > 0 {
		out.ErrorRate = float64(row.ErrorCount) / float64(row.EventCount)
	}
	return out
}
