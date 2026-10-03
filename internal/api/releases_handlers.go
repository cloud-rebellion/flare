package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

type releaseResponse struct {
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	NewIssues int64     `json:"new_issues"`
}

func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListReleasesByProject(r.Context(), generated.ListReleasesByProjectParams{
		ProjectID: chi.URLParam(r, "id"), OrgID: orgIDFrom(r.Context()),
	})
	if err != nil {
		slogError(w, "list releases", err)
		return
	}
	out := make([]releaseResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, releaseResponse{Version: row.Version, CreatedAt: row.CreatedAt.Time, NewIssues: row.NewIssues})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateRelease is the deploy marker: a CI step (or the SDK) registers a
// release so it appears with its deploy time even before any errors arrive.
// Idempotent on (project, version).
func (s *Server) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version string `json:"version"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Version = strings.TrimSpace(req.Version)
	if req.Version == "" {
		writeErr(w, http.StatusBadRequest, "version is required")
		return
	}
	// Verify the path project belongs to the caller's org before writing a
	// release against it. Without this, a member of any org can insert a release
	// row into another org's project (the FK only checks projects(id)), which
	// both writes cross-tenant and, because UpsertRelease is ON CONFLICT DO
	// NOTHING, permanently suppresses the victim's own release record.
	proj, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{
		ID: chi.URLParam(r, "id"), OrgScope: orgIDFrom(r.Context()),
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	marked, err := s.q.MarkReleaseDeploy(r.Context(), generated.MarkReleaseDeployParams{
		ID: id.New(), ProjectID: proj.ID, OrgID: proj.OrgID, Version: req.Version,
	})
	if err != nil {
		slogError(w, "create release", err)
		return
	}
	createdAt := time.Now()
	if marked.CreatedAt.Valid {
		createdAt = marked.CreatedAt.Time
	}
	writeJSON(w, http.StatusCreated, releaseResponse{Version: marked.Version, CreatedAt: createdAt})
}
