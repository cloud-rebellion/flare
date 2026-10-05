package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/ingest"
	"github.com/bright-interaction/flare/internal/sourcemaps"
)

const (
	maxSourceMapBundleBody  = 64 << 20
	maxSourceMapBundleBytes = 64 << 20
	maxSourceMapBundleItems = 512
)

var (
	errSourceMapBundleConflict = errors.New("source map release already contains a different bundle or artifact")
	errSourceMapBundleQuota    = errors.New("source map project quota exceeded")
	errSourceMapBundleMissing  = errors.New("source map bundle or project not found")
)

type sourceMapBundleUpload struct {
	Release   string `json:"release"`
	Digest    string `json:"digest,omitempty"`
	Artifacts []struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	} `json:"artifacts"`
}

type sourceMapBundleItem struct {
	Name    string
	Content string
	SHA256  string
	Size    int64
}

type sourceMapBundleReceipt struct {
	ID            string                    `json:"id"`
	ProjectID     string                    `json:"project_id"`
	Release       string                    `json:"release"`
	Digest        string                    `json:"digest"`
	ArtifactCount int                       `json:"artifact_count"`
	TotalBytes    int64                     `json:"total_bytes"`
	CreatedAt     time.Time                 `json:"created_at"`
	Artifacts     []sourceMapBundleArtifact `json:"artifacts"`
}

type sourceMapBundleArtifact struct {
	artifactResponse
	ContentSHA256 string `json:"content_sha256"`
}

// A bundle is an immutable complete set for one project and release. Its
// transaction inserts every missing map and its receipt together, or none of
// them. This is the CI admission boundary for a deployed browser build.
func (s *Server) handleUploadSourceMapBundle(w http.ResponseWriter, r *http.Request) {
	projectID, orgID := chi.URLParam(r, "id"), orgIDFrom(r.Context())
	// Reject foreign projects before doing up to 64 MiB of parser work.
	if _, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{ID: projectID, OrgScope: orgID}); err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSourceMapBundleBody))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "source map bundle too large (max 64 MiB)")
		return
	}
	if !utf8.Valid(body) {
		writeErr(w, http.StatusBadRequest, "source map bundle must be UTF-8 JSON")
		return
	}
	var req sourceMapBundleUpload
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	release := strings.TrimSpace(req.Release)
	if release == "" || release != req.Release || ingest.SanitizeColumn(release) != release ||
		len(req.Artifacts) == 0 || len(req.Artifacts) > maxSourceMapBundleItems {
		writeErr(w, http.StatusBadRequest, "release and 1 to 512 artifacts with exact names are required")
		return
	}
	items := make([]sourceMapBundleItem, 0, len(req.Artifacts))
	seen := make(map[string]bool, len(req.Artifacts))
	var total int64
	for _, artifact := range req.Artifacts {
		name := strings.TrimSpace(artifact.Name)
		if name == "" || name != artifact.Name || ingest.SanitizeColumn(name) != name || seen[name] {
			writeErr(w, http.StatusBadRequest, "source map artifact names must be unique and exact")
			return
		}
		seen[name] = true
		if len(artifact.Content) > maxArtifactBody {
			writeErr(w, http.StatusRequestEntityTooLarge, "source map artifact exceeds 30 MiB")
			return
		}
		if artifact.Content == "" || strings.ContainsRune(artifact.Content, '\x00') ||
			sourcemaps.Validate([]byte(artifact.Content)) != sourcemaps.ParseStatusValid {
			writeErr(w, http.StatusUnprocessableEntity, "every bundle artifact must contain a valid source map")
			return
		}
		// This column is PostgreSQL TEXT, not JSONB. Valid UTF-8 JSON with no
		// literal NUL is safe to store byte-for-byte. Exact bytes let the CI
		// uploader calculate the same digest without a Go-specific re-marshaler.
		stored := artifact.Content
		total += int64(len(stored))
		if total > maxSourceMapBundleBytes {
			writeErr(w, http.StatusRequestEntityTooLarge, "source map bundle exceeds 64 MiB of stored bytes")
			return
		}
		sum := sha256.Sum256([]byte(stored))
		items = append(items, sourceMapBundleItem{
			Name: name, Content: stored, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(stored)),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	digest := sourceMapBundleDigest(release, items)
	if req.Digest != "" && req.Digest != digest {
		writeErr(w, http.StatusConflict, "source map bundle digest mismatch")
		return
	}
	receipt, replay, err := s.acceptSourceMapBundle(r.Context(), projectID, orgID, release, digest, items, total)
	switch {
	case errors.Is(err, errSourceMapBundleMissing):
		writeErr(w, http.StatusNotFound, "project not found")
	case errors.Is(err, errSourceMapBundleConflict):
		writeErr(w, http.StatusConflict, "release already has a different source map set")
	case errors.Is(err, errSourceMapBundleQuota):
		writeErr(w, http.StatusInsufficientStorage, "project source map quota exceeded")
	case err != nil:
		slogError(w, "accept source map bundle", err)
	default:
		if !replay {
			s.audit(r.Context(), "sourcemap.bundle.upload", release+"/"+digest)
			writeJSON(w, http.StatusCreated, receipt)
		} else {
			writeJSON(w, http.StatusOK, receipt)
		}
	}
}

func writeBundleDigestField(h hash.Hash, s string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(s)))
	_, _ = h.Write(length[:])
	_, _ = io.WriteString(h, s)
}

// The digest binds the exact release, names and stored bytes. The
// length prefixes make concatenation unambiguous; ordering is name-sorted.
func sourceMapBundleDigest(release string, items []sourceMapBundleItem) string {
	h := sha256.New()
	_, _ = io.WriteString(h, "flare-source-map-bundle-v1\x00")
	writeBundleDigestField(h, release)
	ordered := append([]sourceMapBundleItem(nil), items...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	for _, item := range ordered {
		writeBundleDigestField(h, item.Name)
		writeBundleDigestField(h, item.Content)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func (s *Server) acceptSourceMapBundle(parent context.Context, projectID, orgID, release, digest string, items []sourceMapBundleItem, totalBytes int64) (*sourceMapBundleReceipt, bool, error) {
	return s.acceptSourceMapBundleWithLimits(parent, projectID, orgID, release, digest, items, totalBytes, maxArtifactsPerProject, maxArtifactBytesPerProject)
}

func (s *Server) acceptSourceMapBundleWithLimits(parent context.Context, projectID, orgID, release, digest string, items []sourceMapBundleItem, totalBytes, maxArtifacts, maxBytes int64) (*sourceMapBundleReceipt, bool, error) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
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
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id=$1 AND org_id=$2 FOR UPDATE`, projectID, orgID).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, errSourceMapBundleMissing
		}
		return nil, false, err
	}
	var existingID, existingDigest string
	err = tx.QueryRow(ctx, `SELECT id,digest FROM source_map_bundles WHERE project_id=$1 AND org_id=$2 AND release=$3`, projectID, orgID, release).Scan(&existingID, &existingDigest)
	if err == nil {
		if existingDigest != digest {
			return nil, false, errSourceMapBundleConflict
		}
		receipt, err := loadSourceMapBundle(ctx, tx, projectID, orgID, existingID)
		return receipt, true, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}

	var currentCount, currentBytes, releaseCount int64
	if err := tx.QueryRow(ctx, `SELECT count(*)::bigint,coalesce(sum(octet_length(content)),0)::bigint FROM source_map_artifacts WHERE project_id=$1 AND org_id=$2`, projectID, orgID).Scan(&currentCount, &currentBytes); err != nil {
		return nil, false, err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*)::bigint FROM source_map_artifacts WHERE project_id=$1 AND org_id=$2 AND release=$3`, projectID, orgID, release).Scan(&releaseCount); err != nil {
		return nil, false, err
	}
	names := make([]string, len(items))
	byName := make(map[string]sourceMapBundleItem, len(items))
	for i, item := range items {
		names[i] = item.Name
		byName[item.Name] = item
	}
	rows, err := tx.Query(ctx, `SELECT id,name,content,parse_status,created_at FROM source_map_artifacts WHERE project_id=$1 AND org_id=$2 AND release=$3 AND name=ANY($4::text[])`, projectID, orgID, release, names)
	if err != nil {
		return nil, false, err
	}
	existing := make(map[string]artifactResponse, len(items))
	for rows.Next() {
		var id, name, content, parseStatus string
		var createdAt time.Time
		if err := rows.Scan(&id, &name, &content, &parseStatus, &createdAt); err != nil {
			rows.Close()
			return nil, false, err
		}
		item := byName[name]
		if content != item.Content || parseStatus != string(sourcemaps.ParseStatusValid) {
			rows.Close()
			return nil, false, errSourceMapBundleConflict
		}
		existing[name] = artifactResponse{ID: id, Release: release, Name: name, Size: int64(len(content)), ParseStatus: parseStatus, CreatedAt: createdAt}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, false, err
	}
	if releaseCount != int64(len(existing)) {
		return nil, false, errSourceMapBundleConflict // no unlisted live maps may be silently orphaned
	}
	newBytes := int64(0)
	for _, item := range items {
		if _, found := existing[item.Name]; !found {
			newBytes += item.Size
		}
	}
	if currentCount+int64(len(items)-len(existing)) > maxArtifacts || currentBytes+newBytes > maxBytes {
		return nil, false, errSourceMapBundleQuota
	}
	bundleID := id.New()
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `INSERT INTO source_map_bundles (id,project_id,org_id,release,digest,artifact_count,total_bytes) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`, bundleID, projectID, orgID, release, digest, len(items), totalBytes).Scan(&createdAt); err != nil {
		return nil, false, err
	}
	artifacts := make([]sourceMapBundleArtifact, 0, len(items))
	for _, item := range items {
		artifact, found := existing[item.Name]
		if !found {
			artifact.ID = id.New()
			artifact.Release, artifact.Name = release, item.Name
			artifact.Size, artifact.ParseStatus = item.Size, string(sourcemaps.ParseStatusValid)
			if err := tx.QueryRow(ctx, `INSERT INTO source_map_artifacts (id,project_id,org_id,release,name,content,parse_status) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`, artifact.ID, projectID, orgID, release, item.Name, item.Content, artifact.ParseStatus).Scan(&artifact.CreatedAt); err != nil {
				return nil, false, err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO source_map_bundle_items (bundle_id,artifact_id,content_sha256) VALUES ($1,$2,$3)`, bundleID, artifact.ID, item.SHA256); err != nil {
			return nil, false, err
		}
		artifacts = append(artifacts, sourceMapBundleArtifact{artifactResponse: artifact, ContentSHA256: item.SHA256})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return &sourceMapBundleReceipt{ID: bundleID, ProjectID: projectID, Release: release, Digest: digest, ArtifactCount: len(items), TotalBytes: totalBytes, CreatedAt: createdAt, Artifacts: artifacts}, false, nil
}

type sourceMapBundleReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func loadSourceMapBundle(ctx context.Context, q sourceMapBundleReader, projectID, orgID, bundleID string) (*sourceMapBundleReceipt, error) {
	receipt := &sourceMapBundleReceipt{}
	if err := q.QueryRow(ctx, `SELECT id,project_id,release,digest,artifact_count,total_bytes,created_at FROM source_map_bundles WHERE id=$1 AND project_id=$2 AND org_id=$3`, bundleID, projectID, orgID).Scan(&receipt.ID, &receipt.ProjectID, &receipt.Release, &receipt.Digest, &receipt.ArtifactCount, &receipt.TotalBytes, &receipt.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errSourceMapBundleMissing
		}
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT a.id,a.release,a.name,octet_length(a.content)::bigint,a.parse_status,a.created_at,i.content_sha256 FROM source_map_bundle_items i JOIN source_map_artifacts a ON a.id=i.artifact_id WHERE i.bundle_id=$1 ORDER BY a.name COLLATE "C"`, bundleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	receipt.Artifacts = make([]sourceMapBundleArtifact, 0, receipt.ArtifactCount)
	for rows.Next() {
		var item sourceMapBundleArtifact
		if err := rows.Scan(&item.ID, &item.Release, &item.Name, &item.Size, &item.ParseStatus, &item.CreatedAt, &item.ContentSHA256); err != nil {
			return nil, err
		}
		receipt.Artifacts = append(receipt.Artifacts, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(receipt.Artifacts) != receipt.ArtifactCount {
		return nil, errors.New("source map bundle receipt is incomplete")
	}
	return receipt, nil
}

func (s *Server) handleGetSourceMapBundle(w http.ResponseWriter, r *http.Request) {
	receipt, err := loadSourceMapBundle(r.Context(), s.pool, chi.URLParam(r, "id"), orgIDFrom(r.Context()), chi.URLParam(r, "bundleID"))
	switch {
	case errors.Is(err, errSourceMapBundleMissing):
		writeErr(w, http.StatusNotFound, "source map bundle not found")
	case err != nil:
		slogError(w, "get source map bundle", err)
	default:
		writeJSON(w, http.StatusOK, receipt)
	}
}
