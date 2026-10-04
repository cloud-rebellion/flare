package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

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

	// Keep upload compatibility: malformed maps are retained with a bounded
	// parser outcome instead of turning a CI upload into a hard failure. The
	// same parser backs read-time symbolication, so `valid` means the resolver
	// accepted the document and every other status explains why it will remain
	// unmapped. Parser errors never cross the API boundary. Run this after the
	// tenant gate so an unauthorized upload cannot spend parser work on a large
	// body. Normalize before the quota transaction to count the bytes actually
	// stored and keep parsing outside the project lock.
	parseStatus := sourcemaps.Validate([]byte(req.Content))
	artifact := generated.UpsertSourceMapParams{
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
	}
	row, quotaExceeded, err := s.acceptSourceMap(r.Context(), artifact)
	if errors.Is(err, errSourceMapBundleConflict) {
		writeErr(w, http.StatusConflict, "release has an immutable source map bundle")
		return
	}
	if err != nil {
		slogError(w, "accept source map", err)
		return
	}
	if quotaExceeded {
		writeErr(w, http.StatusInsufficientStorage,
			"this project has reached its source map storage limit; delete old releases' maps first")
		return
	}
	s.audit(r.Context(), "sourcemap.upload", row.Release+"/"+row.Name)
	writeJSON(w, http.StatusCreated, artifactResponse{
		ID: row.ID, Release: row.Release, Name: row.Name, Size: int64(row.Size), ParseStatus: row.ParseStatus, CreatedAt: row.CreatedAt.Time,
	})
}

// acceptSourceMap serializes quota admission for every uploader of a project,
// including concurrent requests handled by other Flare replicas. Replacements
// consume only their change in stored bytes and no new artifact slot.
func (s *Server) acceptSourceMap(parent context.Context, artifact generated.UpsertSourceMapParams) (*generated.UpsertSourceMapRow, bool, error) {
	return s.acceptSourceMapWithLimits(parent, artifact, maxArtifactsPerProject, maxArtifactBytesPerProject)
}

func (s *Server) acceptSourceMapWithLimits(parent context.Context, artifact generated.UpsertSourceMapParams, maxArtifacts, maxBytes int64) (*generated.UpsertSourceMapRow, bool, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
		defer done()
		_ = tx.Rollback(cleanup)
	}()
	var projectID string
	if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id=$1 AND org_id=$2 FOR UPDATE`, artifact.ProjectID, artifact.OrgID).Scan(&projectID); err != nil {
		return nil, false, err
	}
	var bundled bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM source_map_bundles WHERE project_id=$1 AND org_id=$2 AND release=$3)`, artifact.ProjectID, artifact.OrgID, artifact.Release).Scan(&bundled); err != nil {
		return nil, false, err
	}
	if bundled {
		return nil, false, errSourceMapBundleConflict
	}
	qtx := generated.New(tx)
	used, err := qtx.GetSourceMapArtifactQuota(ctx, generated.GetSourceMapArtifactQuotaParams{
		ProjectID: artifact.ProjectID, OrgID: artifact.OrgID,
		Release: artifact.Release, Name: artifact.Name,
	})
	if err != nil {
		return nil, false, err
	}
	if used.Artifacts-used.ExistingArtifacts+1 > maxArtifacts ||
		used.Bytes-used.ExistingBytes+int64(len(artifact.Content)) > maxBytes {
		return nil, true, nil
	}
	row, err := qtx.UpsertSourceMap(ctx, artifact)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return row, false, nil
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
	rows, err := s.deleteSourceMap(r.Context(), chi.URLParam(r, "id"), orgIDFrom(r.Context()), chi.URLParam(r, "artifactID"))
	if errors.Is(err, errSourceMapBundleConflict) {
		writeErr(w, http.StatusConflict, "source map belongs to an immutable bundle")
		return
	}
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

// The same project row lock used by single and bundle uploads serializes a
// legacy delete with bundle admission. A bound map can only be retired by a
// future receipt-aware lifecycle operation, not an arbitrary artifact DELETE.
func (s *Server) deleteSourceMap(parent context.Context, projectID, orgID, artifactID string) (int64, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
		defer done()
		_ = tx.Rollback(cleanup)
	}()
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id=$1 AND org_id=$2 FOR UPDATE`, projectID, orgID).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	var release string
	if err := tx.QueryRow(ctx, `SELECT release FROM source_map_artifacts WHERE id=$1 AND project_id=$2 AND org_id=$3`, artifactID, projectID, orgID).Scan(&release); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	var bundled bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM source_map_bundles WHERE project_id=$1 AND org_id=$2 AND release=$3)`, projectID, orgID, release).Scan(&bundled); err != nil {
		return 0, err
	}
	if bundled {
		return 0, errSourceMapBundleConflict
	}
	tag, err := tx.Exec(ctx, `DELETE FROM source_map_artifacts WHERE id=$1 AND project_id=$2 AND org_id=$3`, artifactID, projectID, orgID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
