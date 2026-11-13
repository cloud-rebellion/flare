package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/netguard"
	"github.com/bright-interaction/flare/internal/oidc"
	"github.com/bright-interaction/flare/internal/oidcprofile"
)

type oidcConfigResponse struct {
	Enabled     bool   `json:"enabled"`
	Issuer      string `json:"issuer"`
	ClientID    string `json:"client_id"`
	DefaultRole string `json:"default_role"`
	RedirectURI string `json:"redirect_uri"`
	LoginURL    string `json:"login_url"`
}

func (s *Server) oidcRedirectURI() string {
	return strings.TrimRight(s.cfg.BaseURL, "/") + "/api/auth/oidc/callback"
}

func (s *Server) handleGetOIDCConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org := orgIDFrom(ctx)
	resp := oidcConfigResponse{RedirectURI: s.oidcRedirectURI()}
	if o, err := s.q.GetOrgByID(ctx, org); err == nil {
		resp.LoginURL = strings.TrimRight(s.cfg.BaseURL, "/") + "/api/auth/oidc/start?org=" + o.Slug
	}
	cfg, err := s.q.GetOIDCConfig(ctx, org)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if err != nil {
		slogError(w, "get oidc config", err)
		return
	}
	resp.Enabled = cfg.Enabled
	resp.Issuer = cfg.Issuer
	resp.ClientID = cfg.ClientID
	resp.DefaultRole = cfg.DefaultRole
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSetOIDCConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Issuer       string `json:"issuer"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		DefaultRole  string `json:"default_role"`
		Enabled      bool   `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Issuer = strings.TrimRight(strings.TrimSpace(req.Issuer), "/")
	req.ClientID = strings.TrimSpace(req.ClientID)
	req.ClientSecret = strings.TrimSpace(req.ClientSecret)
	if !strings.HasPrefix(req.Issuer, "https://") || req.ClientID == "" {
		writeErr(w, http.StatusBadRequest, "issuer (https) and client_id are required")
		return
	}
	// Reject issuers whose host is a loopback/private/link-local IP literal
	// at CONFIGURE time, not just at discovery time. The SSRF-guarded dialer
	// already blocks the fetch, but accepting https://169.254.169.254 as a
	// stored issuer passed a live probe on 2026-06-30; validation belongs at
	// the trust boundary too.
	//
	// This check was written HERE and nowhere else, so the BYOAI base_url and
	// the notification webhook URL, which are the two an ordinary member can
	// set, kept accepting anything. One helper, all three surfaces.
	if err := netguard.ValidatePublicURL(req.Issuer); err != nil {
		writeErr(w, http.StatusBadRequest, "issuer "+err.Error())
		return
	}
	// A blank secret on update keeps the stored one (so admins can toggle
	// enabled or change the role without re-entering it). That stored value is
	// already encrypted at rest and is carried through untouched, while a
	// supplied secret is client input and is always encrypted. Different trust
	// levels, different paths: never one call that decides by inspecting the
	// string, which is the decryption oracle described on secretbox.Encrypt.
	storedClientSecret := ""
	if req.ClientSecret == "" {
		existing, err := s.q.GetOIDCConfig(r.Context(), orgIDFrom(r.Context()))
		if err != nil || existing.ClientSecret == "" {
			writeErr(w, http.StatusBadRequest, "a client_secret is required")
			return
		}
		storedClientSecret = existing.ClientSecret
	}
	clientSecretColumn := secretColumnForUpdate(s.secrets, req.ClientSecret, storedClientSecret)
	if req.DefaultRole == "" {
		req.DefaultRole = "member"
	}
	// JIT users must never be auto-elevated to owner.
	if !validRole(req.DefaultRole) || req.DefaultRole == "owner" {
		writeErr(w, http.StatusBadRequest, "default role must be viewer, member or admin")
		return
	}
	if err := s.q.UpsertOIDCConfig(r.Context(), generated.UpsertOIDCConfigParams{
		OrgID:        orgIDFrom(r.Context()),
		Issuer:       req.Issuer,
		ClientID:     req.ClientID,
		ClientSecret: clientSecretColumn,
		DefaultRole:  req.DefaultRole,
		Enabled:      req.Enabled,
	}); err != nil {
		slogError(w, "save oidc config", err)
		return
	}
	s.audit(r.Context(), "sso.configure", req.Issuer)
	s.handleGetOIDCConfig(w, r)
}

func (s *Server) handleDeleteOIDCConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.q.DeleteOIDCConfig(r.Context(), orgIDFrom(r.Context())); err != nil {
		slogError(w, "delete oidc config", err)
		return
	}
	s.audit(r.Context(), "sso.disconnect", "")
	writeJSON(w, http.StatusNoContent, nil)
}

// provisionSSOUser creates a JIT account and binds it to the IdP identity in
// ONE transaction. Doing the two steps separately left an orphan account behind
// whenever the bind failed (most obviously on a `sub` collision), and that
// orphan then matched the email lookup on every later attempt, so the user was
// locked out permanently with no way to recover through the UI.
func (s *Server) provisionSSOUser(ctx context.Context, org, email, issuer, subject, role string) (*generated.User, error) {
	token, err := id.Token(24)
	if err != nil {
		return nil, err
	}
	hash, err := auth.HashPassword(token) // unusable random password; SSO only
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	q := s.q.WithTx(tx)
	user, err := q.CreateUser(ctx, generated.CreateUserParams{
		ID: id.New(), OrgID: org, Email: email, PasswordHash: hash, Role: role,
	})
	if err != nil {
		return nil, err
	}
	n, err := q.LinkUserSSOIdentity(ctx, generated.LinkUserSSOIdentityParams{
		ID: user.ID, SsoIssuer: issuer, SsoSubject: subject,
	})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("sso: identity bind affected no rows for %s", user.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return user, nil
}

// ssoError redirects the browser back to the login page with an error code.
func ssoError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/login?error="+code, http.StatusFound)
}

// handleOIDCStart begins the SSO flow for the org named by ?org=<slug>.
func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org, err := s.q.GetOrgBySlug(ctx, strings.TrimSpace(r.URL.Query().Get("org")))
	if err != nil {
		ssoError(w, r, "sso_unknown_org")
		return
	}
	cfg, err := s.q.GetOIDCConfig(ctx, org.ID)
	if err != nil || !cfg.Enabled {
		ssoError(w, r, "sso_disabled")
		return
	}
	profile, err := s.oidcAssurance(org.ID)
	if err != nil {
		ssoError(w, r, "sso_disabled")
		return
	}
	connectorCtx := integration.WithScope(ctx, integration.Scope{OrgID: org.ID})
	provider, err := oidc.DiscoverWithPolicy(connectorCtx, cfg.Issuer, s.cfg.OIDCPrivateIssuer, s.cfg.OIDCPeerIP)
	if err != nil {
		ssoError(w, r, "sso_discovery")
		return
	}
	s.beginOIDC(w, r, org.ID, cfg, provider, profile)
}

func (s *Server) beginOIDC(w http.ResponseWriter, r *http.Request, org string, cfg *generated.OidcConfig, provider *oidc.Provider, profile oidcprofile.Policy) {
	proof := oidcTransaction{State: oidc.RandomState(), Org: org,
		Issuer: cfg.Issuer, ClientID: cfg.ClientID, RedirectURI: s.oidcRedirectURI(),
		Nonce: oidc.RandomState(), Verifier: oauth2.GenerateVerifier(), Expires: time.Now().Add(oidcTxTTL).Unix()}
	proof.Assurance = s.oidcAssuranceBinding(org, cfg, profile)
	if err := s.storeOIDCTransaction(r.Context(), proof); err != nil {
		ssoError(w, r, "sso_state")
		return
	}
	// Dedicated SameSite=Lax transaction cookie, NOT the Strict session cookie:
	// the browser withholds a Strict cookie on the IdP's redirect back.
	s.setOIDCTx(w, proof)
	http.Redirect(w, r, provider.AuthorizationURLWithAssurance(cfg.ClientID, proof.RedirectURI, proof.State, proof.Verifier, proof.Nonce, profile), http.StatusFound)
}

// handleOIDCCallback completes the SSO flow: validate state, exchange the code,
// read userinfo, match or JIT-provision the user, and sign them in.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	qstate := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	proof, validProof := s.readOIDCTx(r)
	// One-time use: drop the transaction regardless of outcome.
	s.clearOIDCTx(w)

	if code == "" || qstate == "" || !validProof ||
		subtle.ConstantTimeCompare([]byte(qstate), []byte(proof.State)) != 1 || !s.consumeOIDCTransaction(ctx, proof) {
		ssoError(w, r, "sso_state")
		return
	}
	sessOrg := proof.Org
	cfg, err := s.q.GetOIDCConfig(ctx, sessOrg)
	if err != nil || !cfg.Enabled || cfg.Issuer != proof.Issuer || cfg.ClientID != proof.ClientID || s.oidcRedirectURI() != proof.RedirectURI {
		ssoError(w, r, "sso_disabled")
		return
	}
	profile, err := s.oidcAssurance(sessOrg)
	if err != nil || proof.Assurance != s.oidcAssuranceBinding(sessOrg, cfg, profile) {
		ssoError(w, r, "sso_state")
		return
	}
	connectorCtx := integration.WithScope(ctx, integration.Scope{OrgID: sessOrg})
	provider, err := oidc.DiscoverWithPolicy(connectorCtx, cfg.Issuer, s.cfg.OIDCPrivateIssuer, s.cfg.OIDCPeerIP)
	if err != nil {
		ssoError(w, r, "sso_discovery")
		return
	}
	clientSecret, err := s.secrets.Decrypt(cfg.ClientSecret)
	if err != nil {
		slog.Error("sso: stored client secret cannot be decrypted; check FLARE_SECRET_KEY")
		ssoError(w, r, "sso_exchange")
		return
	}
	info, err := provider.AuthenticateWithAssurance(connectorCtx, cfg.ClientID, clientSecret, code, proof.RedirectURI, proof.Verifier, proof.Nonce, profile)
	if err != nil || info == nil || info.Subject == "" {
		ssoError(w, r, "sso_userinfo")
		return
	}
	user, failure := s.resolveOIDCUser(ctx, sessOrg, cfg, info)
	if failure != "" {
		ssoError(w, r, failure)
		return
	}

	s.establishSession(ctx, user.ID, user.OrgID)
	http.Redirect(w, r, "/projects", http.StatusFound)
}

// resolveOIDCUser is called only after ID-token and userinfo subject verification.
func (s *Server) resolveOIDCUser(ctx context.Context, sessOrg string, cfg *generated.OidcConfig, info *oidc.UserInfo) (*generated.User, string) {
	email := strings.ToLower(strings.TrimSpace(info.Email))
	// Resolve by the IdP's STABLE identifier first. `sub` is immutable; the
	// email is not, and it is asserted by whoever controls this org's SSO
	// config. Looking up by email first meant an IdP-side rename missed the
	// existing account, JIT-provisioned a duplicate, and then collided on the
	// (sso_issuer, sso_subject) unique index, locking the user out permanently.
	user, err := s.q.GetUserBySSOIdentity(ctx, generated.GetUserBySSOIdentityParams{
		SsoIssuer: cfg.Issuer, SsoSubject: info.Subject,
	})
	switch {
	case err == nil:
		if user.OrgID != sessOrg {
			return nil, "sso_other_org"
		}
		// Keep the local email in step with the IdP after a rename. Best-effort:
		// a clash with another local account must not block a valid sign-in.
		if info.EmailVerified && email != "" && !strings.EqualFold(user.Email, email) {
			if uerr := s.q.UpdateUserEmail(ctx, generated.UpdateUserEmailParams{ID: user.ID, Email: email}); uerr != nil {
				slog.Warn("sso: could not sync verified renamed email")
			}
		}

	case errors.Is(err, pgx.ErrNoRows):
		// This operator-selected pilot mode never links by email or creates users.
		// It applies only to the explicitly pinned CloudRebellion issuer.
		if s.cfg.OIDCPrelinkedOnly && cfg.Issuer == s.cfg.OIDCPrivateIssuer {
			return nil, "sso_prelinked_required"
		}
		if email == "" {
			return nil, "sso_userinfo"
		}
		if !info.EmailVerified {
			return nil, "sso_unverified"
		}
		// No account bound to this identity yet. Fall back to email.
		user, err = s.q.GetUserByEmail(ctx, email)
		switch {
		case err == nil:
			// Email is globally unique; it must belong to the org that started SSO.
			if user.OrgID != sessOrg {
				return nil, "sso_other_org"
			}
			if user.SsoSubject != "" {
				// Already bound to a DIFFERENT identity (we missed on the
				// identity lookup above). Re-pointing SSO at another IdP must
				// not take over an existing, possibly higher-privileged account.
				s.recordSecurityEvent(user.OrgID, "sso-identity-mismatch",
					"SSO sign-in rejected: account is bound to a different IdP identity", "")
				return nil, "sso_identity_mismatch"
			}
			// First SSO sign-in for this account: bind it now. The row count
			// matters - 0 rows means someone bound it concurrently, so the
			// binding this request would rely on is not the one in the DB.
			n, lerr := s.q.LinkUserSSOIdentity(ctx, generated.LinkUserSSOIdentityParams{
				ID: user.ID, SsoIssuer: cfg.Issuer, SsoSubject: info.Subject,
			})
			if lerr != nil || n == 0 {
				slog.Warn("sso: identity bind did not apply")
				return nil, "sso_link"
			}
			s.audit(ctx, "sso.link", email)

		case errors.Is(err, pgx.ErrNoRows):
			// Just-in-time provisioning with the configured default role.
			// CreateUser + the identity bind run in ONE transaction: a bind
			// failure used to leave an orphan account behind that then blocked
			// every later attempt on the email lookup.
			user, err = s.provisionSSOUser(ctx, sessOrg, email, cfg.Issuer, info.Subject, cfg.DefaultRole)
			if err != nil {
				slog.Warn("sso: provisioning failed")
				return nil, "sso_provision"
			}
			s.audit(ctx, "sso.provision", email)

		default:
			return nil, "sso_lookup"
		}

	default:
		return nil, "sso_lookup"
	}

	return user, ""
}
