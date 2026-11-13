package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

const sourceMapRetirementQuietDays = 30

var (
	errSourceMapRetirementProofUnavailable = errors.New("trusted release protection is unavailable")
	errSourceMapRetirementProtected        = errors.New("source map release is protected or recently used")
	errSourceMapRetirementMissing          = errors.New("source map bundle or project not found")
	errSourceMapRetirementChanged          = errors.New("source map bundle digest changed")
	errSourceMapRetirementIntegrity        = errors.New("source map bundle has incomplete items")
	quotedBundleDigest                     = regexp.MustCompile(`^"sha256:[0-9a-f]{64}"$`)
)

// A fencer is an internal, trusted adapter to the deployment controller. Its
// Acquire call must hold an exclusive project-wide deploy AND rollback fence
// until Release is called, and return an exhaustive snapshot of browser
// release labels on every live target plus every rollback target. It must also
// prove a bounded client/event lifetime: the last time a valid old-release
// event could arrive must precede the 30-day quiet window. A caller's HTTP
// body, release registry row, or issue-scoped deploy receipt cannot supply
// either proof. No adapter is wired today, so the public route fails closed.
type sourceMapRetirementFencer interface {
	Acquire(context.Context, string, string, string) (sourceMapReleaseProtection, error)
}

type sourceMapReleaseProtection struct {
	OrgID, ProjectID string
	CandidateRelease string
	ActiveReleases   []string
	RollbackReleases []string
	Ref, Generation  string
	EventPolicyRef   string
	LastValidEventAt time.Time
	CapturedAt       time.Time
	ReleaseFence     func()
}

func (p sourceMapReleaseProtection) valid(orgID, projectID, release string) bool {
	if p.OrgID != orgID || p.ProjectID != projectID || p.CandidateRelease != release || release == "" ||
		p.Ref == "" || len(p.Ref) > 256 ||
		p.Generation == "" || len(p.Generation) > 128 || p.EventPolicyRef == "" ||
		len(p.EventPolicyRef) > 256 || p.LastValidEventAt.IsZero() || p.ActiveReleases == nil ||
		p.RollbackReleases == nil || p.ReleaseFence == nil || p.CapturedAt.IsZero() {
		return false
	}
	age := time.Since(p.CapturedAt)
	if age < 0 || age > 30*time.Second {
		return false
	}
	if !p.LastValidEventAt.Before(time.Now().UTC().AddDate(0, 0, -sourceMapRetirementQuietDays)) {
		return false
	}
	for _, release := range append(slices.Clone(p.ActiveReleases), p.RollbackReleases...) {
		if release == "" {
			return false
		}
	}
	return true
}

func (p sourceMapReleaseProtection) protects(release string) bool {
	return slices.Contains(p.ActiveReleases, release) || slices.Contains(p.RollbackReleases, release)
}

type sourceMapRetirementReceipt struct {
	ID                   string    `json:"id"`
	ProjectID            string    `json:"project_id"`
	BundleID             string    `json:"bundle_id"`
	Release              string    `json:"release"`
	Digest               string    `json:"digest"`
	ArtifactCount        int       `json:"artifact_count"`
	TotalBytes           int64     `json:"total_bytes"`
	ProtectionRef        string    `json:"protection_ref"`
	ProtectionGeneration string    `json:"protection_generation"`
	EventPolicyRef       string    `json:"event_policy_ref"`
	LastValidEventAt     time.Time `json:"last_valid_event_at"`
	RetiredAt            time.Time `json:"retired_at"`
}

func scanSourceMapRetirement(row pgx.Row) (*sourceMapRetirementReceipt, error) {
	receipt := &sourceMapRetirementReceipt{}
	err := row.Scan(&receipt.ID, &receipt.ProjectID, &receipt.BundleID, &receipt.Release, &receipt.Digest,
		&receipt.ArtifactCount, &receipt.TotalBytes, &receipt.ProtectionRef,
		&receipt.ProtectionGeneration, &receipt.EventPolicyRef,
		&receipt.LastValidEventAt, &receipt.RetiredAt)
	return receipt, err
}

func loadSourceMapRetirementByBundle(ctx context.Context, pool *pgxpool.Pool, orgID, projectID, bundleID string) (*sourceMapRetirementReceipt, error) {
	return scanSourceMapRetirement(pool.QueryRow(ctx, `SELECT id,project_id,bundle_id,release,digest,
		artifact_count,total_bytes,protection_ref,protection_generation,event_policy_ref,last_valid_event_at,retired_at
		FROM source_map_bundle_retirements WHERE org_id=$1 AND project_id=$2 AND bundle_id=$3`,
		orgID, projectID, bundleID))
}

func (s *Server) handleGetSourceMapRetirement(w http.ResponseWriter, r *http.Request) {
	receipt, err := scanSourceMapRetirement(s.pool.QueryRow(r.Context(), `SELECT id,project_id,bundle_id,release,digest,
		artifact_count,total_bytes,protection_ref,protection_generation,event_policy_ref,last_valid_event_at,retired_at
		FROM source_map_bundle_retirements WHERE org_id=$1 AND project_id=$2 AND id=$3`,
		orgIDFrom(r.Context()), chi.URLParam(r, "id"), chi.URLParam(r, "retirementID")))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "source map retirement not found")
		return
	}
	if err != nil {
		slogError(w, "get source map retirement", err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

// The only destructive bundle route requires an interactive administrator and
// an exact digest precondition. A trusted fencer is required independently of
// those human inputs. Its absence leaves every bundle and map untouched.
func (s *Server) handleRetireSourceMapBundle(w http.ResponseWriter, r *http.Request) {
	if authSourceFrom(r.Context()) != "session" || userIDFrom(r.Context()) == "" {
		writeErr(w, http.StatusForbidden, "interactive administrator required")
		return
	}
	orgID, projectID, bundleID := orgIDFrom(r.Context()), chi.URLParam(r, "id"), chi.URLParam(r, "bundleID")
	if _, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{ID: projectID, OrgScope: orgID}); err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	if r.Header.Get("If-Match") == "" {
		writeErr(w, http.StatusPreconditionRequired, "quoted bundle digest If-Match is required")
		return
	}
	if !quotedBundleDigest.MatchString(r.Header.Get("If-Match")) {
		writeErr(w, http.StatusBadRequest, "If-Match must be a quoted source map bundle digest")
		return
	}
	digest := r.Header.Get("If-Match")[1 : len(r.Header.Get("If-Match"))-1]
	prior, err := loadSourceMapRetirementByBundle(r.Context(), s.pool, orgID, projectID, bundleID)
	if err == nil {
		if prior.Digest != digest {
			writeErr(w, http.StatusPreconditionFailed, "source map bundle digest changed")
		} else {
			writeJSON(w, http.StatusOK, prior)
		}
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slogError(w, "check source map retirement replay", err)
		return
	}
	if s.sourceMapRetirementFencer == nil {
		writeErr(w, http.StatusServiceUnavailable, "trusted release and event-lifetime protection is unavailable")
		return
	}
	var candidateRelease, candidateDigest string
	if err := s.pool.QueryRow(r.Context(), `SELECT release,digest FROM source_map_bundles
		WHERE id=$1 AND project_id=$2 AND org_id=$3`, bundleID, projectID, orgID).Scan(&candidateRelease, &candidateDigest); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "source map bundle not found")
		} else {
			slogError(w, "load source map bundle for retirement", err)
		}
		return
	}
	if candidateDigest != digest {
		writeErr(w, http.StatusPreconditionFailed, "source map bundle digest changed")
		return
	}
	proof, err := s.sourceMapRetirementFencer.Acquire(r.Context(), orgID, projectID, candidateRelease)
	if proof.ReleaseFence != nil {
		defer proof.ReleaseFence()
	}
	if err != nil || !proof.valid(orgID, projectID, candidateRelease) {
		writeErr(w, http.StatusServiceUnavailable, "trusted release and event-lifetime protection is unavailable")
		return
	}
	receipt, err := s.retireSourceMapBundle(r.Context(), orgID, projectID, bundleID,
		digest, userIDFrom(r.Context()), proof)
	switch {
	case errors.Is(err, errSourceMapRetirementProofUnavailable):
		writeErr(w, http.StatusServiceUnavailable, "retirement proof or recent event history is unavailable")
	case errors.Is(err, errSourceMapRetirementMissing):
		// A concurrent request may have committed while we waited for the
		// project lock. An exact receipt is a safe idempotent replay.
		prior, replayErr := loadSourceMapRetirementByBundle(r.Context(), s.pool, orgID, projectID, bundleID)
		if replayErr == nil && prior.Digest == digest {
			writeJSON(w, http.StatusOK, prior)
		} else if replayErr == nil {
			writeErr(w, http.StatusPreconditionFailed, "source map bundle digest changed")
		} else if errors.Is(replayErr, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "source map bundle not found")
		} else {
			slogError(w, "check source map retirement replay", replayErr)
		}
	case errors.Is(err, errSourceMapRetirementChanged):
		writeErr(w, http.StatusPreconditionFailed, "source map bundle digest changed")
	case errors.Is(err, errSourceMapRetirementProtected):
		writeErr(w, http.StatusConflict, "release is live, a rollback target, recently deployed, or recently used")
	case err != nil:
		slogError(w, "retire source map bundle", err)
	default:
		writeJSON(w, http.StatusOK, receipt)
	}
}

// retireSourceMapBundle locks the same project row as both upload paths and
// checks protected releases and retained hot events inside the deletion
// transaction. Its audit entry and receipt are mandatory: either both commit
// with the map deletion or all changes roll back. The caller holds the external
// deployment fence through this function's return.
func (s *Server) retireSourceMapBundle(parent context.Context, orgID, projectID, bundleID, digest, actor string, proof sourceMapReleaseProtection) (*sourceMapRetirementReceipt, error) {
	if !proof.valid(orgID, projectID, proof.CandidateRelease) || actor == "" {
		return nil, errSourceMapRetirementProofUnavailable
	}
	// The quiet-period proof needs the hot window that is actually in force,
	// which the dashboard may have lowered below the env value.
	if posture := s.retentionPosture(parent); !posture.Known || posture.Policy.HotDays < sourceMapRetirementQuietDays {
		return nil, errSourceMapRetirementProofUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
		defer done()
		_ = tx.Rollback(cleanup)
	}()
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id=$1 AND org_id=$2 FOR UPDATE`, projectID, orgID).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errSourceMapRetirementMissing
		}
		return nil, err
	}
	receipt := &sourceMapRetirementReceipt{ID: id.New(), ProjectID: projectID, BundleID: bundleID,
		ProtectionRef: proof.Ref, ProtectionGeneration: proof.Generation,
		EventPolicyRef: proof.EventPolicyRef, LastValidEventAt: proof.LastValidEventAt}
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `SELECT release,digest,artifact_count,total_bytes,created_at
		FROM source_map_bundles WHERE id=$1 AND project_id=$2 AND org_id=$3 FOR UPDATE`,
		bundleID, projectID, orgID).Scan(&receipt.Release, &receipt.Digest, &receipt.ArtifactCount, &receipt.TotalBytes, &createdAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errSourceMapRetirementMissing
		}
		return nil, err
	}
	if receipt.Digest != digest {
		return nil, errSourceMapRetirementChanged
	}
	if receipt.Release != proof.CandidateRelease {
		return nil, errSourceMapRetirementProofUnavailable
	}
	if proof.protects(receipt.Release) {
		return nil, errSourceMapRetirementProtected
	}
	// Use PostgreSQL's clock for the final cutoff: event reception and bundle
	// creation timestamps are DB-owned, while the API host may have clock skew.
	var quietSince time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp() - interval '30 days'`).Scan(&quietSince); err != nil {
		return nil, err
	}
	if !createdAt.Before(quietSince) || !proof.LastValidEventAt.Before(quietSince) {
		return nil, errSourceMapRetirementProtected
	}
	var recent bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM events WHERE project_id=$1 AND org_id=$2 AND release=$3 AND received_at >= $4
		UNION ALL
		SELECT 1 FROM releases WHERE project_id=$1 AND org_id=$2 AND version=$3
		AND (deploy_marked_at >= $4 OR last_heartbeat_at >= $4)
	)`, projectID, orgID, receipt.Release, quietSince).Scan(&recent); err != nil {
		return nil, err
	}
	if recent {
		return nil, errSourceMapRetirementProtected
	}
	var itemCount, scopedCount, bytes int64
	if err := tx.QueryRow(ctx, `SELECT count(*)::bigint,
		count(*) FILTER (WHERE a.project_id=$2 AND a.org_id=$3 AND a.release=$4)::bigint,
		coalesce(sum(octet_length(a.content)) FILTER
			(WHERE a.project_id=$2 AND a.org_id=$3 AND a.release=$4),0)::bigint
		FROM source_map_bundle_items i JOIN source_map_artifacts a ON a.id=i.artifact_id
		WHERE i.bundle_id=$1`, bundleID, projectID, orgID, receipt.Release).Scan(&itemCount, &scopedCount, &bytes); err != nil {
		return nil, err
	}
	if itemCount != int64(receipt.ArtifactCount) || scopedCount != itemCount || bytes != receipt.TotalBytes {
		return nil, errSourceMapRetirementIntegrity
	}
	// The item-to-artifact reference is deferred. Deleting the bundle below
	// cascades its items before commit, satisfying the deferred check.
	tag, err := tx.Exec(ctx, `DELETE FROM source_map_artifacts WHERE id IN (
		SELECT artifact_id FROM source_map_bundle_items WHERE bundle_id=$1)`, bundleID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != itemCount {
		return nil, errSourceMapRetirementIntegrity
	}
	if _, err := tx.Exec(ctx, `DELETE FROM source_map_bundles WHERE id=$1 AND project_id=$2 AND org_id=$3`, bundleID, projectID, orgID); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO source_map_bundle_retirements
		(id,org_id,project_id,bundle_id,release,digest,artifact_count,total_bytes,actor_user_id,
		 protection_ref,protection_generation,event_policy_ref,last_valid_event_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING last_valid_event_at,retired_at`,
		receipt.ID, orgID, projectID, bundleID, receipt.Release, receipt.Digest, receipt.ArtifactCount,
		receipt.TotalBytes, actor, proof.Ref, proof.Generation, proof.EventPolicyRef,
		proof.LastValidEventAt).Scan(&receipt.LastValidEventAt, &receipt.RetiredAt); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log (id,org_id,actor_user_id,action,target)
		VALUES ($1,$2,$3,'sourcemap.bundle.retire',$4)`, id.New(), orgID, actor, bundleID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return receipt, nil
}
