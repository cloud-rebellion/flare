package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/hubspot"
	"github.com/bright-interaction/flare/internal/id"
)

const (
	hubspotTxCookie     = "flare_hubspot_tx"
	hubspotCallbackPath = "/api/connectors/hubspot/callback"
	hubspotTxTTL        = 10 * time.Minute
)

type hubspotOAuthTx struct {
	OrgID, UserID, ProjectID string
	StartedAt                time.Time
}

func (s *Server) hubspotOAuthReady() bool {
	return s.hubspotOAuth.Configured() && s.secrets != nil && s.secrets.Enabled() && s.pool != nil
}

func (s *Server) handleHubSpotOAuthAvailability(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"available": s.hubspotOAuthReady(),
		"scope":     []string{"oauth"},
	})
}

// The CSRF-protected POST has a real Flare admin session. A separate Lax
// transaction cookie carries browser binding across HubSpot's cross-site GET
// redirect, where Flare's Strict session cookie is intentionally absent.
func (s *Server) handleHubSpotOAuthStart(w http.ResponseWriter, r *http.Request) {
	if !s.hubspotOAuthReady() {
		writeErr(w, http.StatusServiceUnavailable, "HubSpot OAuth is not configured")
		return
	}
	if authSourceFrom(r.Context()) != "session" || userIDFrom(r.Context()) == "" {
		writeErr(w, http.StatusForbidden, "a human administrator session is required")
		return
	}
	var req struct {
		ProjectID string `json:"project_id"`
	}
	if err := decodeJSON(r, &req); err != nil || req.ProjectID == "" || len(req.ProjectID) > 128 || !connectorConnectionToken.MatchString(req.ProjectID) {
		writeErr(w, http.StatusBadRequest, "a valid project_id is required")
		return
	}
	if _, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{ID: req.ProjectID, OrgScope: orgIDFrom(r.Context())}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusBadRequest, "project is not in this workspace")
		} else {
			writeErr(w, http.StatusServiceUnavailable, "project lookup is unavailable")
		}
		return
	}
	state, err := id.Token(32)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "HubSpot OAuth is unavailable")
		return
	}
	cookieValue, err := id.Token(32)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "HubSpot OAuth is unavailable")
		return
	}
	authURL, err := s.hubspotOAuth.AuthorizationURL(state)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "HubSpot OAuth is unavailable")
		return
	}
	stateHash := sha256.Sum256([]byte(state))
	cookieHash := sha256.Sum256([]byte(cookieValue))
	// Bounded, opportunistic cleanup; consumed and expired state carries no
	// credential and has no reason to persist after the retry window.
	_, _ = s.pool.Exec(r.Context(), `DELETE FROM hubspot_oauth_transactions WHERE expires_at < now() - interval '1 day'`)
	dbtx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "HubSpot OAuth is unavailable")
		return
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
		defer done()
		_ = dbtx.Rollback(cleanup)
	}()
	_, err = dbtx.Exec(r.Context(), `INSERT INTO hubspot_oauth_transactions
        (state_hash, cookie_hash, org_id, user_id, project_id, expires_at)
        VALUES ($1,$2,$3,$4,$5,now()+interval '10 minutes')`,
		stateHash[:], cookieHash[:], orgIDFrom(r.Context()), userIDFrom(r.Context()), req.ProjectID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "HubSpot OAuth is unavailable")
		return
	}
	// Starting a consent flow changes durable security state. Keep the audit
	// row in the same transaction, and never record the state, cookie or URL.
	qtx := generated.New(dbtx)
	if err := qtx.InsertAuditLog(r.Context(), generated.InsertAuditLogParams{
		ID: id.New(), OrgID: orgIDFrom(r.Context()), ActorUserID: pgText(userIDFrom(r.Context())),
		Action: "connector.oauth_start", Target: req.ProjectID,
	}); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "HubSpot OAuth is unavailable")
		return
	}
	if err := dbtx.Commit(r.Context()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "HubSpot OAuth is unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: hubspotTxCookie, Value: cookieValue, Path: hubspotCallbackPath,
		MaxAge: int(hubspotTxTTL.Seconds()), HttpOnly: true,
		Secure: s.cfg.IsProduction(), SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"authorization_url": authURL})
}

func (s *Server) clearHubSpotTx(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: hubspotTxCookie, Value: "", Path: hubspotCallbackPath,
		MaxAge: -1, HttpOnly: true, Secure: s.cfg.IsProduction(), SameSite: http.SameSiteLaxMode,
	})
}

func hubspotOAuthReturn(w http.ResponseWriter, r *http.Request, code string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, "/settings?hubspot="+code, http.StatusSeeOther)
}

func (s *Server) consumeHubSpotTx(ctx context.Context, state, cookieValue string) (hubspotOAuthTx, error) {
	if len(state) != 64 || len(cookieValue) != 64 {
		return hubspotOAuthTx{}, errors.New("invalid oauth transaction")
	}
	stateHash, cookieHash := sha256.Sum256([]byte(state)), sha256.Sum256([]byte(cookieValue))
	var tx hubspotOAuthTx
	err := s.pool.QueryRow(ctx, `UPDATE hubspot_oauth_transactions SET consumed_at=now()
        WHERE state_hash=$1 AND cookie_hash=$2 AND consumed_at IS NULL AND expires_at>now()
        RETURNING org_id, user_id, project_id, started_at`, stateHash[:], cookieHash[:]).
		Scan(&tx.OrgID, &tx.UserID, &tx.ProjectID, &tx.StartedAt)
	if err != nil {
		return hubspotOAuthTx{}, errors.New("invalid oauth transaction")
	}
	return tx, nil
}

// saveHubSpotOAuth binds a grant to the project the administrator selected at
// start. Re-consent atomically replaces the account's credential and moves its
// existing connection ID to that project when necessary. Historical receipts
// keep their original project; new checks use the newly selected one. Never
// revoke a newly issued token on conflict: HubSpot may rotate a single grant
// for the account and revoking it could disable the existing installation.
func (s *Server) saveHubSpotOAuth(ctx context.Context, attempt hubspotOAuthTx, tokens hubspot.OAuthTokens) (string, string, error) {
	dbtx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "save_failed", err
	}
	defer dbtx.Rollback(ctx) //nolint:errcheck
	var ignored string
	if err := dbtx.QueryRow(ctx, `SELECT id FROM orgs WHERE id=$1 FOR UPDATE`, attempt.OrgID).Scan(&ignored); err != nil {
		return "", "access_changed", err
	}
	var role, userOrg string
	var validFrom *time.Time
	if err := dbtx.QueryRow(ctx, `SELECT org_id, role, sessions_valid_from FROM users WHERE id=$1 FOR SHARE`, attempt.UserID).
		Scan(&userOrg, &role, &validFrom); err != nil || userOrg != attempt.OrgID || !roleAtLeast(role, "admin") ||
		validFrom != nil && validFrom.After(attempt.StartedAt) {
		return "", "access_changed", errors.New("HubSpot installer access changed")
	}
	if err := dbtx.QueryRow(ctx, `SELECT id FROM projects WHERE id=$1 AND org_id=$2 FOR SHARE`, attempt.ProjectID, attempt.OrgID).Scan(&ignored); err != nil {
		return "", "project_changed", err
	}
	hubID := strconv.FormatInt(tokens.HubID, 10)
	var connectionID, projectID, status, ciphertext string
	err = dbtx.QueryRow(ctx, `SELECT id, COALESCE(project_id,''), status, secret_ciphertext
		FROM connector_connections WHERE org_id=$1 AND provider='hubspot' AND installation_id=$2 FOR UPDATE`, attempt.OrgID, hubID).
		Scan(&connectionID, &projectID, &status, &ciphertext)
	newConnection := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !newConnection {
		return "", "save_failed", err
	}
	if !newConnection {
		if status == "revoked" {
			return "", "revocation_pending", errors.New("HubSpot revocation is pending")
		}
		old, _, oauthGrant, err := hubspot.DecodeOAuthCredential(s.secrets, ciphertext)
		if err != nil || oauthGrant && (old.OrgID != attempt.OrgID || old.ProjectID != projectID || old.ConnectionID != connectionID || old.HubID != tokens.HubID) {
			return "", "existing_connection_conflict", errors.New("HubSpot account is already connected with another credential")
		}
	} else {
		connectionID = id.New()
	}
	stored, err := hubspot.NewOAuthCredential(attempt.OrgID, attempt.ProjectID, connectionID, tokens, time.Now()).Encode(s.secrets)
	if err != nil {
		return "", "save_failed", err
	}
	if newConnection {
		_, err = dbtx.Exec(ctx, `INSERT INTO connector_connections
			(id,org_id,project_id,provider,installation_id,installation_name,status,scopes,secret_ciphertext)
			VALUES ($1,$2,$3,'hubspot',$4,$5,'active','["oauth"]'::jsonb,$6)`,
			connectionID, attempt.OrgID, attempt.ProjectID, hubID, "HubSpot account "+hubID, stored)
	} else {
		_, err = dbtx.Exec(ctx, `UPDATE connector_connections SET project_id=$3,status='active',scopes='["oauth"]'::jsonb,
			secret_ciphertext=$4,updated_at=now() WHERE id=$1 AND org_id=$2`, connectionID, attempt.OrgID, attempt.ProjectID, stored)
	}
	if err != nil {
		return "", "save_failed", err
	}
	if !newConnection && projectID != attempt.ProjectID {
		// A prior success belongs to the old project. Do not show it as fresh
		// health for the newly selected project after this atomic move.
		if _, err := dbtx.Exec(ctx, `UPDATE connector_sync_states SET state='idle',
			last_success_at=NULL,last_completed_at=NULL,last_error_code='',
			next_run_at=NULL,lease_owner='',lease_expires_at=NULL,updated_at=now()
			WHERE org_id=$1 AND connection_id=$2`, attempt.OrgID, connectionID); err != nil {
			return "", "save_failed", err
		}
	}
	if err := dbtx.Commit(ctx); err != nil {
		return "", "save_failed", err
	}
	if !newConnection && projectID != attempt.ProjectID {
		return connectionID, "moved", nil
	}
	return connectionID, "connected", nil
}

func (s *Server) handleHubSpotOAuthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	cookie, err := r.Cookie(hubspotTxCookie)
	s.clearHubSpotTx(w)
	state := r.URL.Query().Get("state")
	if !s.hubspotOAuthReady() || err != nil || len(state) != 64 || len(cookie.Value) != 64 {
		hubspotOAuthReturn(w, r, "state_error")
		return
	}
	tx, err := s.consumeHubSpotTx(r.Context(), state, cookie.Value)
	if err != nil {
		hubspotOAuthReturn(w, r, "state_error")
		return
	}
	// A removed/demoted administrator or a session revoked since start cannot
	// complete a consent flow merely by possessing the Lax transaction cookie.
	user, err := s.q.GetUserByID(r.Context(), tx.UserID)
	if err != nil || user.OrgID != tx.OrgID || !roleAtLeast(user.Role, "admin") ||
		user.SessionsValidFrom.Valid && user.SessionsValidFrom.Time.After(tx.StartedAt) {
		hubspotOAuthReturn(w, r, "access_changed")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" || r.URL.Query().Get("error") != "" || !s.hubspotOAuthReady() {
		hubspotOAuthReturn(w, r, "authorization_failed")
		return
	}
	tokens, err := s.hubspotOAuth.Exchange(r.Context(), code)
	if err != nil {
		hubspotOAuthReturn(w, r, "exchange_failed")
		return
	}
	// Health-only consent must not retain a broader grant than this install
	// requested. HubSpot's oauth scope allows account-details without CRM data.
	if len(tokens.Scopes) != 1 || tokens.Scopes[0] != "oauth" {
		hubspotOAuthReturn(w, r, "scope_changed")
		return
	}
	connectionID, resultCode, err := s.saveHubSpotOAuth(r.Context(), tx, tokens)
	if err != nil {
		hubspotOAuthReturn(w, r, resultCode)
		return
	}
	auditCtx := context.WithValue(context.WithValue(r.Context(), ctxOrgID, tx.OrgID), ctxUserID, tx.UserID)
	action := "connector.connect"
	if resultCode == "moved" {
		action = "connector.move"
	}
	s.audit(auditCtx, action, connectionID)
	hubspotOAuthReturn(w, r, resultCode)
}
