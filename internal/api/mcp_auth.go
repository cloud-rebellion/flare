package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
)

const mcpEndpoint = "/api/mcp"

// Bearer authentication does not use ambient cookies. Cookie-only browser
// calls retain CSRF; anonymous clients reach the real 401 challenge instead of
// being rejected by a browser CSRF check before authentication discovery.
func (s *Server) mcpTransport(csrfMW func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		protected := csrfMW(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if !s.validMCPOrigin(r) {
				writeErr(w, http.StatusForbidden, "MCP origin is not allowed")
				return
			}
			if len(r.Header.Values("Authorization")) > 0 || !hasSessionCookie(r) {
				next.ServeHTTP(w, r)
				return
			}
			protected.ServeHTTP(w, r)
		})
	}
}

func (s *Server) allowedMCPTools(ctx context.Context) []string {
	names := []string{}
	for _, tool := range s.mcpToolset() {
		if !tool.RequiresWrite || roleAtLeast(roleFrom(ctx), "member") {
			names = append(names, tool.Name)
		}
	}
	sort.Strings(names)
	return names
}

// Metadata describes only the authenticated caller's authority. It never
// returns a bearer, session ID, key hash, provider credential or raw telemetry.
func (s *Server) mcpSetupInfo(ctx context.Context) map[string]any {
	return map[string]any{
		"server_name": "flare", "release": s.cfg.Release, "endpoint": strings.TrimRight(s.cfg.BaseURL, "/") + mcpEndpoint,
		"protocol_version": mcpProtocol, "auth_methods": []string{"bearer", "session"}, "oauth_supported": false,
		"mcp_token": map[string]any{"purpose": "mcp", "default_role": "viewer", "default_expires_in_days": 30, "max_expires_in_days": 90},
		"identity": map[string]any{
			"org_id": orgIDFrom(ctx), "role": roleFrom(ctx), "auth_source": authSourceFrom(ctx),
			"credential_purpose": credentialPurposeFrom(ctx), "expires_at": credentialExpiryFrom(ctx),
		},
		"scope": "all_projects_in_workspace", "allowed_tools": s.allowedMCPTools(ctx),
	}
}

func (s *Server) handleMCPStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if orgIDFrom(r.Context()) == "" {
		s.authenticationRequired(w)
		return
	}
	writeJSON(w, http.StatusOK, s.mcpSetupInfo(r.Context()))
}
