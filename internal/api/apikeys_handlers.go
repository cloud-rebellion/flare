package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

type apiKeyResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Role       string     `json:"role"`
	Purpose    string     `json:"purpose"`
	ExpiresAt  *time.Time `json:"expires_at"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// handleCreateAPIKey mints an org-scoped API key. The plaintext is returned
// exactly once; only its hash is stored.
func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if authSourceFrom(r.Context()) != "session" || userIDFrom(r.Context()) == "" || !roleAtLeast(roleFrom(r.Context()), "admin") {
		writeErr(w, http.StatusForbidden, "a human administrator session is required")
		return
	}
	var req struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"`
		Role          string `json:"role"`
		Purpose       string `json:"purpose"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "API key"
	}
	if len(name) > 128 {
		writeErr(w, http.StatusBadRequest, "key name must be at most 128 bytes")
		return
	}
	purpose := req.Purpose
	if purpose == "" {
		purpose = "api"
	}
	if purpose != "api" && purpose != "mcp" {
		writeErr(w, http.StatusBadRequest, "purpose must be api or mcp")
		return
	}
	if purpose == "mcp" {
		if req.ExpiresInDays == 0 {
			req.ExpiresInDays = 30
		}
		if req.ExpiresInDays < 1 || req.ExpiresInDays > 90 {
			writeErr(w, http.StatusBadRequest, "MCP credentials must expire in 1 to 90 days")
			return
		}
	}
	// Least privilege is the DEFAULT for a new key. Every key used to act as a
	// member, which includes irreversible project deletion, so a token minted
	// for a dashboard could erase a project. Callers opt UP to member
	// explicitly; owner/admin are not mintable at all, because a long-lived
	// token must never be able to change who can log in.
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = "viewer"
	}
	if role != "viewer" && role != "member" {
		writeErr(w, http.StatusBadRequest, "role must be viewer or member")
		return
	}

	plaintext, hash, prefix, err := auth.GenerateAPIKey()
	if err != nil {
		slogError(w, "generate api key", err)
		return
	}
	var expires pgtype.Timestamptz
	if req.ExpiresInDays > 0 {
		expires = pgtype.Timestamptz{Time: time.Now().AddDate(0, 0, req.ExpiresInDays), Valid: true}
	}

	creator := pgtype.Text{String: userIDFrom(r.Context()), Valid: true}
	var k *generated.ApiKey
	if purpose == "mcp" {
		k, err = s.q.CreateMCPAPIKey(r.Context(), generated.CreateMCPAPIKeyParams{
			ID: id.New(), OrgID: orgIDFrom(r.Context()), Name: name, KeyHash: hash, KeyPrefix: prefix,
			ExpiresInDays: int32(req.ExpiresInDays), Role: role, CreatedByUserID: creator,
		})
	} else {
		k, err = s.q.CreateAPIKey(r.Context(), generated.CreateAPIKeyParams{
			ID: id.New(), OrgID: orgIDFrom(r.Context()), Name: name,
			KeyHash: hash, KeyPrefix: prefix, ExpiresAt: expires, Role: role, CreatedByUserID: creator,
		})
	}
	if err != nil {
		slogError(w, "create api key", err)
		return
	}
	s.audit(r.Context(), "apikey.create", k.Name)
	// key (plaintext) is shown once and never again.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": k.ID, "name": k.Name, "prefix": k.KeyPrefix, "role": k.Role, "key": plaintext,
		"purpose": k.Purpose, "expires_at": optionalKeyExpiry(k.ExpiresAt),
	})
}

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	keys, err := s.q.ListAPIKeysByOrg(r.Context(), orgIDFrom(r.Context()))
	if err != nil {
		slogError(w, "list api keys", err)
		return
	}
	out := make([]apiKeyResponse, 0, len(keys))
	for _, k := range keys {
		var last *time.Time
		if k.LastUsedAt.Valid {
			t := k.LastUsedAt.Time
			last = &t
		}
		out = append(out, apiKeyResponse{
			ID: k.ID, Name: k.Name, Prefix: k.KeyPrefix, Role: k.Role,
			Purpose: k.Purpose, ExpiresAt: optionalKeyExpiry(k.ExpiresAt),
			CreatedBy: k.CreatedByEmail.String, CreatedAt: k.CreatedAt.Time, LastUsedAt: last,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteAPIKey revokes an org-scoped API key immediately.
func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if authSourceFrom(r.Context()) != "session" || userIDFrom(r.Context()) == "" || !roleAtLeast(roleFrom(r.Context()), "admin") {
		writeErr(w, http.StatusForbidden, "a human administrator session is required")
		return
	}
	rows, err := s.q.DeleteAPIKey(r.Context(), generated.DeleteAPIKeyParams{
		ID:    chi.URLParam(r, "id"),
		OrgID: orgIDFrom(r.Context()),
	})
	if err != nil {
		slogError(w, "delete api key", err)
		return
	}
	if rows == 0 {
		writeErr(w, http.StatusNotFound, "api key not found")
		return
	}
	s.audit(r.Context(), "apikey.revoke", chi.URLParam(r, "id"))
	writeJSON(w, http.StatusNoContent, nil)
}

func optionalKeyExpiry(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}
