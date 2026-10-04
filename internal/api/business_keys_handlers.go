package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

const defaultBusinessKeyExpiryDays = 90
const maxBusinessKeyExpiryDays = 365
const maxActiveBusinessKeysPerProject = 8

type businessKeyMetadata struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

func (s *Server) businessKeyProject(w http.ResponseWriter, r *http.Request) (*generated.Project, bool) {
	// The router requires admin, which an org API key cannot reach. Repeat the
	// human-actor guard here so direct or future routes cannot mint or revoke a
	// credential whose durable audit row has no accountable session user.
	if userIDFrom(r.Context()) == "" {
		writeErr(w, http.StatusForbidden, "human administrator session required")
		return nil, false
	}
	project, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{
		ID: chi.URLParam(r, "id"), OrgScope: orgIDFrom(r.Context()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "project not found")
		return nil, false
	}
	if err != nil {
		slogError(w, "get project for business key", err)
		return nil, false
	}
	return project, true
}

// An administrator provisions a confidential, project-scoped reporter key.
// The 192-bit bearer token appears only in this no-store response; a prefix
// and SHA-256 lookup hash are the only representations kept by Flare.
func (s *Server) handleCreateBusinessCheckKey(w http.ResponseWriter, r *http.Request) {
	project, ok := s.businessKeyProject(w, r)
	if !ok {
		return
	}
	var req struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Business reporter"
	}
	if len(name) > 64 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		writeErr(w, http.StatusBadRequest, "name must be at most 64 printable bytes")
		return
	}
	days := req.ExpiresInDays
	if days == 0 {
		days = defaultBusinessKeyExpiryDays
	}
	if days < 1 || days > maxBusinessKeyExpiryDays {
		writeErr(w, http.StatusBadRequest, "expires_in_days must be between 1 and 365")
		return
	}
	randomPart, err := id.Token(24)
	if err != nil {
		slogError(w, "generate business key", err)
		return
	}
	plaintext := "flb_" + randomPart
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		slogError(w, "begin business key creation", err)
		return
	}
	defer rollbackBusinessKeyTx(r.Context(), tx)
	qtx := generated.New(tx)
	// Lock the project row before counting so concurrent admins cannot each
	// observe the same spare slot and exceed the active-key cap.
	if _, err := qtx.LockProjectForBusinessKeyCreation(r.Context(), generated.LockProjectForBusinessKeyCreationParams{
		ID: project.ID, OrgID: project.OrgID,
	}); errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	} else if err != nil {
		slogError(w, "lock project for business key creation", err)
		return
	}
	activeKeys, err := qtx.CountActiveBusinessCheckKeys(r.Context(), generated.CountActiveBusinessCheckKeysParams{
		OrgID: project.OrgID, ProjectID: project.ID,
	})
	if err != nil {
		slogError(w, "count active business keys", err)
		return
	}
	if activeKeys >= maxActiveBusinessKeysPerProject {
		writeErr(w, http.StatusConflict, "active business key limit reached; revoke an unused key")
		return
	}
	created, err := qtx.CreateBusinessCheckKey(r.Context(), generated.CreateBusinessCheckKeyParams{
		ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID, Name: name,
		KeyHash: auth.HashAPIKey(plaintext), KeyPrefix: plaintext[:12],
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().UTC().AddDate(0, 0, days), Valid: true},
	})
	if err != nil {
		slogError(w, "create business key", err)
		return
	}
	if err := qtx.InsertAuditLog(r.Context(), generated.InsertAuditLogParams{
		ID: id.New(), OrgID: project.OrgID, ActorUserID: businessKeyAuditActor(r.Context()),
		Action: "business_key.create", Target: project.ID + "/" + created.ID,
	}); err != nil {
		slogError(w, "audit business key creation", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		slogError(w, "commit business key creation", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": created.ID, "name": created.Name, "prefix": created.KeyPrefix,
		"created_at": created.CreatedAt.Time, "expires_at": created.ExpiresAt.Time,
		"key": plaintext,
	})
}

func (s *Server) handleListBusinessCheckKeys(w http.ResponseWriter, r *http.Request) {
	project, ok := s.businessKeyProject(w, r)
	if !ok {
		return
	}
	rows, err := s.q.ListBusinessCheckKeys(r.Context(), generated.ListBusinessCheckKeysParams{
		OrgID: project.OrgID, ProjectID: project.ID,
	})
	if err != nil {
		slogError(w, "list business keys", err)
		return
	}
	out := make([]businessKeyMetadata, 0, len(rows))
	for _, row := range rows {
		out = append(out, businessKeyMetadata{
			ID: row.ID, Name: row.Name, Prefix: row.KeyPrefix,
			CreatedAt: row.CreatedAt.Time, ExpiresAt: row.ExpiresAt.Time,
			RevokedAt: receiptTimestamp(row.RevokedAt),
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// Rotation is create-new, update the backend, verify one accepted check, then
// revoke-old. Revocation is soft so historical checks keep their key ID.
func (s *Server) handleRevokeBusinessCheckKey(w http.ResponseWriter, r *http.Request) {
	project, ok := s.businessKeyProject(w, r)
	if !ok {
		return
	}
	keyID := chi.URLParam(r, "keyID")
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		slogError(w, "begin business key revocation", err)
		return
	}
	defer rollbackBusinessKeyTx(r.Context(), tx)
	qtx := generated.New(tx)
	rows, err := qtx.RevokeBusinessCheckKey(r.Context(), generated.RevokeBusinessCheckKeyParams{
		ID: keyID, OrgID: project.OrgID, ProjectID: project.ID,
	})
	if err != nil {
		slogError(w, "revoke business key", err)
		return
	}
	if rows == 0 {
		writeErr(w, http.StatusNotFound, "business key not found")
		return
	}
	if err := qtx.InsertAuditLog(r.Context(), generated.InsertAuditLogParams{
		ID: id.New(), OrgID: project.OrgID, ActorUserID: businessKeyAuditActor(r.Context()),
		Action: "business_key.revoke", Target: project.ID + "/" + keyID,
	}); err != nil {
		slogError(w, "audit business key revocation", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		slogError(w, "commit business key revocation", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func businessKeyAuditActor(ctx context.Context) pgtype.Text {
	return pgtype.Text{String: userIDFrom(ctx), Valid: true}
}

func rollbackBusinessKeyTx(ctx context.Context, tx pgx.Tx) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(cleanupCtx)
}

// Charge every attempt against its source IP before charging a valid-shaped
// token. This prevents an attacker from rotating random token values to buy
// unlimited database lookups. The token bucket is a fixed-size hash, never the
// bearer secret; public DSN headers do not affect either bucket.
func (s *Server) rateLimitBusinessCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.ingestLimiter.Allow("business-ip:" + clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		if token := bearerToken(r); businessBearerToken.MatchString(token) {
			if !s.ingestLimiter.Allow("business-key:" + auth.HashAPIKey(token)) {
				w.Header().Set("Retry-After", "60")
				writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
