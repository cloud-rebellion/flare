package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/oidc"
	"github.com/bright-interaction/flare/internal/oidcprofile"
	"github.com/jackc/pgx/v5"
)

var errOIDCLaunchUnready = errors.New("OIDC portal launch unavailable")

type oidcLaunchTarget struct {
	org      string
	config   *generated.OidcConfig
	provider *oidc.Provider
	profile  oidcprofile.Policy
}

func (s *Server) oidcLaunchTarget(ctx context.Context) (*oidcLaunchTarget, error) {
	selected := s.cfg.OIDCLaunchOrgID
	if !oidcLaunchOrgPattern.MatchString(selected) {
		return nil, errOIDCLaunchUnready
	}
	profile, err := s.oidcAssurance(selected)
	if err != nil || !profile.Enabled() {
		return nil, errOIDCLaunchUnready
	}
	if s.q == nil {
		return nil, errNativeSessionUnavailable
	}
	// Only the configured primary key chooses a workspace. No request parameter,
	// slug, display name or caller-supplied return URL participates.
	org, err := s.q.GetOrgByID(ctx, selected)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errOIDCLaunchUnready
	}
	if err != nil {
		return nil, err
	}
	cfg, err := s.q.GetOIDCConfig(ctx, org.ID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !cfg.Enabled {
		return nil, errOIDCLaunchUnready
	}
	if err != nil {
		return nil, err
	}
	connector := integration.WithScope(ctx, integration.Scope{OrgID: org.ID})
	provider, err := oidc.DiscoverWithPolicy(connector, cfg.Issuer, s.cfg.OIDCPrivateIssuer, s.cfg.OIDCPeerIP)
	if err != nil {
		return nil, err
	}
	if !provider.SupportsACR(profile.RequiredACR) {
		return nil, errOIDCLaunchUnready
	}
	return &oidcLaunchTarget{org: org.ID, config: cfg, provider: provider, profile: profile}, nil
}

func (s *Server) handleOIDCLaunchStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, err := s.oidcLaunchTarget(r.Context())
	status := http.StatusOK
	if err != nil && !errors.Is(err, errOIDCLaunchUnready) {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]bool{"ready": err == nil})
}

func (s *Server) handleOIDCLaunch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, err := s.nativeSessionUser(r.Context()); err == nil {
		// Another tab may have signed in after the frontend's signed-out probe.
		http.Redirect(w, r, "/projects", http.StatusFound)
		return
	} else if !errors.Is(err, errNativeSessionRequired) {
		writeErr(w, http.StatusServiceUnavailable, "session could not be checked")
		return
	}
	target, err := s.oidcLaunchTarget(r.Context())
	if err != nil {
		if errors.Is(err, errOIDCLaunchUnready) {
			ssoError(w, r, "sso_disabled")
		} else {
			writeErr(w, http.StatusServiceUnavailable, "SSO is unavailable; use local sign-in")
		}
		return
	}
	s.beginOIDC(w, r, target.org, target.config, target.provider, target.profile)
}
