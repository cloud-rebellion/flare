package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
)

// releaseSymbolicationHealthResponse is a payload-free projection of the
// source-map coverage query. It reports whether the resolver can select an
// uploaded artifact for a generated frame; map content, event bodies, and
// stack traces never cross this endpoint. A nil coverage rate means the
// release had no frames eligible for source-map matching in the window.
type releaseSymbolicationHealthResponse struct {
	Version              string     `json:"version"`
	CreatedAt            *time.Time `json:"created_at,omitempty"`
	MapCount             int64      `json:"map_count"`
	MapBytes             int64      `json:"map_bytes"`
	EventCount           int64      `json:"event_count"`
	StacktraceEventCount int64      `json:"stacktrace_event_count"`
	CandidateFrameCount  int64      `json:"candidate_frame_count"`
	MappedFrameCount     int64      `json:"mapped_frame_count"`
	UnmappedFrameCount   int64      `json:"unmapped_frame_count"`
	CoverageRate         *float64   `json:"coverage_rate,omitempty"`
	FirstSeen            *time.Time `json:"first_seen,omitempty"`
	LastSeen             *time.Time `json:"last_seen,omitempty"`
}

// handleListReleaseSymbolicationHealth returns bounded, tenant-scoped release
// and source-map coverage. It uses the same hot-window and row bounds as
// release health, keeping a dashboard request predictable even for projects
// with many releases or large source-map uploads.
func (s *Server) handleListReleaseSymbolicationHealth(w http.ResponseWriter, r *http.Request) {
	hours := releaseHealthHours(r)
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	rows, err := s.q.ListReleaseSymbolicationHealthByProject(r.Context(), generated.ListReleaseSymbolicationHealthByProjectParams{
		ProjectID: chi.URLParam(r, "id"),
		OrgID:     orgIDFrom(r.Context()),
		Since:     pgtype.Timestamptz{Time: since, Valid: true},
		RowLimit:  int32(releaseHealthLimit(r)),
	})
	if err != nil {
		slogError(w, "list release symbolication health", err)
		return
	}
	out := make([]releaseSymbolicationHealthResponse, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		out = append(out, toReleaseSymbolicationHealthResponse(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func toReleaseSymbolicationHealthResponse(row *generated.ListReleaseSymbolicationHealthByProjectRow) releaseSymbolicationHealthResponse {
	out := releaseSymbolicationHealthResponse{
		Version:              row.Version,
		CreatedAt:            receiptTimestamp(row.CreatedAt),
		MapCount:             row.MapCount,
		MapBytes:             row.MapBytes,
		EventCount:           row.EventCount,
		StacktraceEventCount: row.StacktraceEventCount,
		CandidateFrameCount:  row.CandidateFrameCount,
		MappedFrameCount:     row.MappedFrameCount,
		UnmappedFrameCount:   row.UnmappedFrameCount,
		FirstSeen:            receiptTimestamp(row.FirstSeen),
		LastSeen:             receiptTimestamp(row.LastSeen),
	}
	if row.CandidateFrameCount > 0 {
		rate := float64(row.MappedFrameCount) / float64(row.CandidateFrameCount)
		out.CoverageRate = &rate
	}
	return out
}
