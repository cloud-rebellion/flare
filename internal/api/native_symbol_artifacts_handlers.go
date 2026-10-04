package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/native_symbols"
)

const (
	// Native symbol bundles can be larger than a source map, but they are still
	// CI artifacts rather than an ingest stream. Keep both the decoded artifact
	// and the JSON/base64 request bounded before either reaches Postgres.
	maxNativeSymbolBytes        = 30 << 20
	maxNativeSymbolRequestBytes = (maxNativeSymbolBytes*4)/3 + (1 << 20)
	maxNativeSymbolArtifacts    = 2000
	maxNativeSymbolStorageBytes = 2 << 30
	maxNativeSymbolList         = 2000
	maxNativeSymbolHealthRows   = 100
)

var (
	nativeSymbolReleaseToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+:@/-]{0,127}$`)
	nativeSymbolNameToken    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+@:-]{0,127}$`)
)

// nativeSymbolUploadRequest is intentionally base64 wrapped so a CI caller can
// upload both UTF-8 mapping.txt/.symbols files and binary dSYM/ELF bundles
// through the same authenticated JSON API. The decoded bytes stay in the
// native-symbol artifact table and are never copied into event payloads.
type nativeSymbolUploadRequest struct {
	Release  string `json:"release"`
	Platform string `json:"platform"`
	Format   string `json:"format"`
	Name     string `json:"name"`
	Content  string `json:"content_base64"`
}

type nativeSymbolArtifactResponse struct {
	ID       string `json:"id"`
	Release  string `json:"release"`
	Platform string `json:"platform"`
	Format   string `json:"format"`
	Name     string `json:"name"`
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
	// Present only on a successful upload, after the artifact and terminal
	// acceptance receipt have committed together.
	ReceiptID string `json:"receipt_id,omitempty"`
	// ValidationStatus records what Flare proved about the retained bytes at
	// upload time. It is independent from native frame symbolication, which is
	// still explicitly unresolved until a format-specific resolver is enabled.
	ValidationStatus string    `json:"validation_status"`
	CreatedAt        time.Time `json:"created_at"`
}

type nativeSymbolHealthResponse struct {
	nativeSymbolArtifactResponse
	// SymbolicationStatus is separate from upload/retention status. An accepted
	// artifact is durable evidence, not proof that native frames are readable.
	SymbolicationStatus string `json:"symbolication_status"`
}

// handleUploadNativeSymbolArtifact stores one Flutter/Dart/Android/iOS debug
// artifact for a release. It records a server-side SHA-256 digest and bounded
// metadata, but no source or symbol content is ever returned by this API.
// Actual frame rewriting remains an explicit follow-up per format; accepting
// an artifact must not make Flare claim that a crash is symbolicated.
func (s *Server) handleUploadNativeSymbolArtifact(w http.ResponseWriter, r *http.Request) {
	var outcome ingestOutcome
	requestOutcome := "rejected"
	defer func() {
		outcome.record("symbols")
		recordIngestRequest("native", outcome, requestOutcome)
	}()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxNativeSymbolRequestBytes))
	if err != nil {
		outcome.invalid = 1
		writeErr(w, http.StatusRequestEntityTooLarge, "symbol artifact request is too large")
		return
	}
	var req nativeSymbolUploadRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		outcome.invalid = 1
		writeErr(w, http.StatusBadRequest, "invalid symbol artifact request")
		return
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		outcome.invalid = 1
		writeErr(w, http.StatusBadRequest, "invalid symbol artifact request")
		return
	}
	normalized, content, err := normalizeNativeSymbolUpload(req)
	if err != nil {
		outcome.invalid = 1
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Check the project before quota or upsert work. This mirrors source-map
	// uploads and ensures a project id from another organization cannot be used
	// to probe quotas or overwrite a same-key artifact.
	project, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{
		ID: chi.URLParam(r, "id"), OrgScope: orgIDFrom(r.Context()),
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	receipt := newIngestReceipt(w, r, project, "symbols", "native")
	if receipt == nil || s.pool == nil {
		outcome.failed = 1
		requestOutcome = "failed"
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	receipt.setReceived(1)
	digest := sha256.Sum256(content)
	validationStatus := nativeSymbolValidationStatus(normalized.Platform, normalized.Format, content)
	artifact := generated.UpsertNativeSymbolArtifactParams{
		ID: id.New(), ProjectID: project.ID, OrgID: project.OrgID,
		Release: normalized.Release, Platform: normalized.Platform,
		Format: normalized.Format, Name: normalized.Name,
		Digest: hex.EncodeToString(digest[:]), Size: int64(len(content)), Content: content,
		ValidationStatus: validationStatus,
	}
	row, quotaExceeded, err := s.acceptNativeSymbolArtifact(r.Context(), receipt, artifact)
	if err != nil {
		// An uncertain commit is a retryable failure. Never claim a 201 or
		// complete a detached receipt after the transaction has failed.
		slog.Error("native symbol acceptance transaction failed", "project_id", project.ID)
		outcome.failed = 1
		requestOutcome = "failed"
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusServiceUnavailable, "telemetry acceptance is temporarily unavailable")
		return
	}
	if quotaExceeded {
		outcome.invalid = 1
		writeErr(w, http.StatusInsufficientStorage, "this project has reached its native symbol storage limit")
		return
	}
	// Keep the release marker best-effort and after the artifact commit. A
	// registry hiccup cannot turn retained symbols into a failed upload.
	_ = s.q.UpsertRelease(r.Context(), generated.UpsertReleaseParams{
		ID: id.New(), ProjectID: project.ID, OrgID: project.OrgID, Version: normalized.Release,
	})
	outcome.accepted = 1
	requestOutcome = ""
	s.audit(r.Context(), "native-symbol.upload", row.Release+"/"+row.Platform+"/"+row.Format+"/"+row.Name)
	response := toNativeSymbolArtifactResponse(row)
	response.ReceiptID = receipt.id
	writeJSON(w, http.StatusCreated, response)
}

// acceptNativeSymbolArtifact is the 201 boundary. The project row lock
// serializes quota checks across replicas, including replacements that grow an
// existing artifact. The retained bytes and terminal receipt commit together.
func (s *Server) acceptNativeSymbolArtifact(parent context.Context, receipt *ingestReceipt, artifact generated.UpsertNativeSymbolArtifactParams) (*generated.UpsertNativeSymbolArtifactRow, bool, error) {
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
	if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id=$1 AND org_id=$2 FOR UPDATE`, receipt.projectID, receipt.orgID).Scan(&projectID); err != nil {
		return nil, false, err
	}
	qtx := generated.New(tx)
	used, err := qtx.GetNativeSymbolArtifactQuota(ctx, generated.GetNativeSymbolArtifactQuotaParams{
		ProjectID: receipt.projectID, OrgID: receipt.orgID,
		Release: artifact.Release, Platform: artifact.Platform, Format: artifact.Format, Name: artifact.Name,
	})
	if err != nil {
		return nil, false, err
	}
	nextCount := used.Artifacts
	if used.ExistingBytes == 0 {
		nextCount++
	}
	if nextCount > maxNativeSymbolArtifacts || used.Bytes-used.ExistingBytes+artifact.Size > maxNativeSymbolStorageBytes {
		if err := insertTerminalNativeSymbolReceipt(ctx, qtx, receipt, false); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}
	row, err := qtx.UpsertNativeSymbolArtifact(ctx, artifact)
	if err != nil {
		return nil, false, err
	}
	if err := insertTerminalNativeSymbolReceipt(ctx, qtx, receipt, true); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return row, false, nil
}

func insertTerminalNativeSymbolReceipt(ctx context.Context, q *generated.Queries, receipt *ingestReceipt, accepted bool) error {
	now := time.Now().UTC()
	params := generated.InsertIngestReceiptParams{
		ID: receipt.id, RequestID: receipt.requestID, OrgID: receipt.orgID, ProjectID: receipt.projectID,
		Pillar: receipt.pillar, Protocol: receipt.protocol, ReceivedCount: receipt.received,
		ReceivedAt:  pgtype.Timestamptz{Time: now, Valid: true},
		CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
	if accepted {
		params.AcceptedCount = 1
		params.PersistedCount = 1
		params.Outcome = "accepted"
		params.HttpStatus = http.StatusCreated
	} else {
		params.RejectedCount = 1
		params.Outcome = "rejected"
		params.HttpStatus = http.StatusInsufficientStorage
	}
	_, err := q.InsertIngestReceipt(ctx, params)
	return err
}

func (s *Server) handleListNativeSymbolArtifacts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListNativeSymbolArtifactsByProject(r.Context(), generated.ListNativeSymbolArtifactsByProjectParams{
		ProjectID: chi.URLParam(r, "id"), OrgID: orgIDFrom(r.Context()), Limit: maxNativeSymbolList,
	})
	if err != nil {
		slogError(w, "list native symbol artifacts", err)
		return
	}
	out := make([]nativeSymbolArtifactResponse, 0, len(rows))
	for _, row := range rows {
		if row != nil {
			out = append(out, nativeSymbolArtifactResponse{
				ID: row.ID, Release: row.Release, Platform: row.Platform,
				Format: row.Format, Name: row.Name, Digest: row.Digest,
				Size: row.Size, ValidationStatus: row.ValidationStatus, CreatedAt: row.CreatedAt.Time,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleListNativeSymbolHealth exposes bounded, metadata-only native symbol
// readiness. It intentionally reports retained_unresolved for every format
// until a format-specific resolver has been implemented and verified; no
// artifact bytes or inferred frame locations cross this endpoint.
func (s *Server) handleListNativeSymbolHealth(w http.ResponseWriter, r *http.Request) {
	limit := int32(maxNativeSymbolHealthRows)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxNativeSymbolHealthRows {
			limit = int32(n)
		}
	}
	rows, err := s.q.ListNativeSymbolArtifactsByProject(r.Context(), generated.ListNativeSymbolArtifactsByProjectParams{
		ProjectID: chi.URLParam(r, "id"), OrgID: orgIDFrom(r.Context()), Limit: limit,
	})
	if err != nil {
		slogError(w, "list native symbol health", err)
		return
	}
	out := make([]nativeSymbolHealthResponse, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		out = append(out, nativeSymbolHealthResponse{
			nativeSymbolArtifactResponse: nativeSymbolArtifactResponse{
				ID: row.ID, Release: row.Release, Platform: row.Platform,
				Format: row.Format, Name: row.Name, Digest: row.Digest,
				Size: row.Size, ValidationStatus: row.ValidationStatus, CreatedAt: row.CreatedAt.Time,
			},
			SymbolicationStatus: string(native_symbols.StatusRetainedUnresolved),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteNativeSymbolArtifact(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.DeleteNativeSymbolArtifact(r.Context(), generated.DeleteNativeSymbolArtifactParams{
		ID: chi.URLParam(r, "artifactID"), ProjectID: chi.URLParam(r, "id"), OrgID: orgIDFrom(r.Context()),
	})
	if err != nil {
		slogError(w, "delete native symbol artifact", err)
		return
	}
	if rows == 0 {
		writeErr(w, http.StatusNotFound, "native symbol artifact not found")
		return
	}
	s.audit(r.Context(), "native-symbol.delete", chi.URLParam(r, "artifactID"))
	writeJSON(w, http.StatusNoContent, nil)
}

func normalizeNativeSymbolUpload(req nativeSymbolUploadRequest) (nativeSymbolUploadRequest, []byte, error) {
	req.Release = strings.TrimSpace(req.Release)
	if !nativeSymbolReleaseToken.MatchString(req.Release) {
		return req, nil, errors.New("release is required and must be a safe bounded token")
	}
	req.Platform = strings.ToLower(strings.TrimSpace(req.Platform))
	switch req.Platform {
	case "flutter", "android", "ios":
	default:
		return req, nil, errors.New("platform must be flutter, android, or ios")
	}
	req.Format = strings.ToLower(strings.TrimSpace(req.Format))
	switch req.Format {
	case "dart_symbols", "proguard", "breakpad", "dsym", "elf":
	default:
		return req, nil, errors.New("format must be dart_symbols, proguard, breakpad, dsym, or elf")
	}
	// Keep format/platform pairs explicit. This prevents a typo in CI from
	// making a valid artifact invisible to the eventual platform-specific
	// resolver, while still allowing the Android/iOS native formats that share
	// Breakpad or ELF containers.
	switch {
	case req.Format == "dart_symbols" && req.Platform != "flutter":
		return req, nil, errors.New("format dart_symbols requires flutter")
	case req.Format == "proguard" && req.Platform != "android":
		return req, nil, errors.New("format proguard requires android")
	case req.Format == "dsym" && req.Platform != "ios":
		return req, nil, errors.New("format dsym requires ios")
	case req.Format == "elf" && req.Platform != "android":
		return req, nil, errors.New("format elf requires android")
	}
	req.Name = strings.TrimSpace(req.Name)
	if !nativeSymbolNameToken.MatchString(req.Name) {
		return req, nil, errors.New("name is required and must be a safe bounded token")
	}
	if req.Content == "" {
		return req, nil, errors.New("content_base64 is required")
	}
	content, err := base64.StdEncoding.Strict().DecodeString(req.Content)
	if err != nil {
		return req, nil, errors.New("content_base64 is not valid base64")
	}
	if len(content) == 0 || len(content) > maxNativeSymbolBytes {
		return req, nil, errors.New("decoded symbol artifact is outside the supported size")
	}
	return req, content, nil
}

func toNativeSymbolArtifactResponse(row *generated.UpsertNativeSymbolArtifactRow) nativeSymbolArtifactResponse {
	return nativeSymbolArtifactResponse{
		ID: row.ID, Release: row.Release, Platform: row.Platform,
		Format: row.Format, Name: row.Name, Digest: row.Digest,
		Size: row.Size, ValidationStatus: row.ValidationStatus, CreatedAt: row.CreatedAt.Time,
	}
}

// nativeSymbolValidationStatus converts the parser's internal result into the
// stable API vocabulary. Formats without a resolver remain not_checked: the
// absence of a parser is not evidence that their bytes are malformed.
func nativeSymbolValidationStatus(platform, format string, content []byte) string {
	v := native_symbols.ValidateArtifact(platform, format, content)
	switch v.Status {
	case native_symbols.StatusRecognized:
		return "recognized"
	case native_symbols.StatusInvalid:
		return "invalid"
	case native_symbols.StatusUnsupported:
		return "unsupported"
	default:
		return "not_checked"
	}
}
