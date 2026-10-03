package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/ingest"
	"github.com/bright-interaction/flare/internal/sourcemaps"
)

// maxArtifactBody bounds a source map upload. Maps for large bundles can be
// several MiB, so this is well above the 1 MiB ingest cap.
const maxArtifactBody = 30 << 20

type artifactResponse struct {
	ID          string    `json:"id"`
	Release     string    `json:"release"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	ParseStatus string    `json:"parse_status"`
	CreatedAt   time.Time `json:"created_at"`
}

// handleUploadSourceMap stores (or replaces) one minified file's source map for
// a project + release. The `name` is the minified file as it appears in a stack
// frame (e.g. "app.min.js"); `content` is the .map JSON.
const (
	// maxArtifactsPerProject bounds the row count. A real bundle ships a
	// handful of maps per release and a project keeps a few releases' worth.
	maxArtifactsPerProject = 2000
	// maxArtifactBytesPerProject bounds the volume, which is what actually
	// fills a disk: maxArtifactBody is 30 MiB per upload.
	maxArtifactBytesPerProject = 2 << 30 // 2 GiB
)

func (s *Server) handleUploadSourceMap(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxArtifactBody))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "source map too large (max 30 MiB)")
		return
	}
	var req struct {
		Release string `json:"release"`
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Release = strings.TrimSpace(req.Release)
	req.Name = strings.TrimSpace(req.Name)
	if req.Release == "" || req.Name == "" || req.Content == "" {
		writeErr(w, http.StatusBadRequest, "release, name and content are required")
		return
	}
	if !strings.HasPrefix(strings.TrimSpace(req.Content), "{") {
		writeErr(w, http.StatusBadRequest, "content must be a source map JSON document")
		return
	}
	// Verify the project belongs to the caller's org BEFORE upserting (mirror the
	// monitors/metrics handlers). Without this a tenant could pass another org's project
	// id in the URL and overwrite that project's sourcemap (cross-tenant write), since the
	// upsert conflict targets (project_id, release, name).
	if _, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{
		ID: chi.URLParam(r, "id"), OrgScope: orgIDFrom(r.Context()),
	}); err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}

	// Per-project quota. Sourcemap upload is the only pipeline with no ingest
	// rate limit (it is an auth route) and it deliberately sits at MEMBER so CI
	// keeps working, so a leaked CI key was an unbounded write of 30 MiB
	// artifacts with (project_id, release, name) all caller-chosen: it fills the
	// volume. Counted before the upsert, and an upsert over an existing
	// (release, name) is allowed through so redeploying the same release never
	// wedges.
	if used, cerr := s.q.CountSourceMapsByProject(r.Context(), generated.CountSourceMapsByProjectParams{
		ProjectID: chi.URLParam(r, "id"), OrgID: orgIDFrom(r.Context()),
	}); cerr != nil {
		slogError(w, "count source maps", cerr)
		return
	} else if used.Artifacts >= maxArtifactsPerProject || used.Bytes+int64(len(req.Content)) > maxArtifactBytesPerProject {
		writeErr(w, http.StatusInsufficientStorage,
			"this project has reached its source map storage limit; delete old releases' maps first")
		return
	}

	// Keep upload compatibility: malformed maps are retained with a bounded
	// parser outcome instead of turning a CI upload into a hard failure. The
	// same parser backs read-time symbolication, so `valid` means the resolver
	// accepted the document and every other status explains why it will remain
	// unmapped. Parser errors never cross the API boundary. Run this after the
	// tenant and quota gates so an unauthorized or rejected upload cannot spend
	// parser work on a large body.
	parseStatus := sourcemaps.Validate([]byte(req.Content))

	row, err := s.q.UpsertSourceMap(r.Context(), generated.UpsertSourceMapParams{
		ID:        id.New(),
		ProjectID: chi.URLParam(r, "id"),
		OrgID:     orgIDFrom(r.Context()),
		// Sanitised like every other pipeline. This was the one that was not:
		// a \u0000 inside a source map's sourcesContent fails the INSERT, so
		// the upload 500s forever with no diagnostic and no way for the
		// uploader to know what is wrong. SanitizeJSON parse-remarshals, so an
		// escaped NUL is removed without corrupting the document.
		Release:     ingest.SanitizeColumn(req.Release),
		Name:        ingest.SanitizeColumn(req.Name),
		Content:     string(ingest.SanitizeJSON([]byte(req.Content))),
		ParseStatus: string(parseStatus),
	})
	if err != nil {
		slogError(w, "upsert source map", err)
		return
	}
	s.audit(r.Context(), "sourcemap.upload", row.Release+"/"+row.Name)
	writeJSON(w, http.StatusCreated, artifactResponse{
		ID: row.ID, Release: row.Release, Name: row.Name, Size: int64(row.Size), ParseStatus: row.ParseStatus, CreatedAt: row.CreatedAt.Time,
	})
}

func (s *Server) handleListSourceMaps(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListSourceMapsByProject(r.Context(), generated.ListSourceMapsByProjectParams{
		ProjectID: chi.URLParam(r, "id"), OrgID: orgIDFrom(r.Context()),
	})
	if err != nil {
		slogError(w, "list source maps", err)
		return
	}
	out := make([]artifactResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, artifactResponse{ID: row.ID, Release: row.Release, Name: row.Name, Size: int64(row.Size), ParseStatus: row.ParseStatus, CreatedAt: row.CreatedAt.Time})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteSourceMap(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.DeleteSourceMap(r.Context(), generated.DeleteSourceMapParams{
		ID:        chi.URLParam(r, "artifactID"),
		ProjectID: chi.URLParam(r, "id"),
		OrgID:     orgIDFrom(r.Context()),
	})
	if err != nil {
		slogError(w, "delete source map", err)
		return
	}
	if rows == 0 {
		writeErr(w, http.StatusNotFound, "source map not found")
		return
	}
	s.audit(r.Context(), "sourcemap.delete", chi.URLParam(r, "artifactID"))
	writeJSON(w, http.StatusNoContent, nil)
}
