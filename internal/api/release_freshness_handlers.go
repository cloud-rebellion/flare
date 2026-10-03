package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
)

// releaseFreshnessResponse is a payload-free projection of explicit deploy
// markers and canonical service-up heartbeat age. Releases that were only
// auto-created by event traffic never enter this response.
type releaseFreshnessResponse struct {
	Version         string     `json:"version"`
	DeployMarkedAt  *time.Time `json:"deploy_marked_at,omitempty"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	AgeSeconds      float64    `json:"age_seconds"`
}

// handleListReleaseFreshness returns bounded, tenant/project-scoped release
// freshness. The window limits deploy-marker history; it is not used to infer
// liveness from arbitrary old events.
func (s *Server) handleListReleaseFreshness(w http.ResponseWriter, r *http.Request) {
	hours := releaseHealthHours(r)
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	rows, err := s.q.ListReleaseFreshnessByProject(r.Context(), generated.ListReleaseFreshnessByProjectParams{
		ProjectID: chi.URLParam(r, "id"),
		OrgID:     orgIDFrom(r.Context()),
		Since:     pgtype.Timestamptz{Time: since, Valid: true},
		RowLimit:  int32(releaseHealthLimit(r)),
	})
	if err != nil {
		slogError(w, "list release freshness", err)
		return
	}
	out := make([]releaseFreshnessResponse, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.AgeSeconds < 0 {
			continue
		}
		out = append(out, releaseFreshnessResponse{
			Version:         row.Version,
			DeployMarkedAt:  releaseFreshnessTimestamp(row.DeployMarkedAt),
			LastHeartbeatAt: releaseFreshnessTimestamp(row.LastHeartbeatAt),
			AgeSeconds:      row.AgeSeconds,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func releaseFreshnessTimestamp(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}
