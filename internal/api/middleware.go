package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bright-interaction/flare/internal/auth"
)

// securityHeaders sets the baseline response headers required by the repo
// security rules. csp is the Content-Security-Policy computed once at startup
// (see cspForBuild): scripts are locked to 'self' plus the exact hashes of the
// SPA's inline bootstrap, so the strict policy still lets the dashboard start.
func securityHeaders(csp string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Content-Security-Policy", csp)
			// Set HSTS in-app so a self-host behind any proxy is protected, not only
			// deployments that happen to add it at the edge. Browsers ignore HSTS
			// received over plain HTTP, so this is a no-op in local dev.
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), interest-cohort=()")
			next.ServeHTTP(w, r)
		})
	}
}

// cspForBuild derives the Content-Security-Policy from the embedded frontend.
// SvelteKit's SPA shell boots from an INLINE <script>, which a bare
// `default-src 'self'` blocks (inline scripts need 'unsafe-inline', a nonce,
// or a hash), leaving a blank page. We hash that inline script from the SAME
// bytes the server serves, so script-src stays 'self' + exact hashes with no
// 'unsafe-inline' and no manual step across builds. If the HTML can't be read
// or has no inline script (should not happen), we fall back to script
// 'unsafe-inline' so the UI still works, and log it loudly.
func cspForBuild(build fs.FS) string {
	html, err := fs.ReadFile(build, "index.html")
	if err != nil {
		slog.Warn("csp: index.html unreadable; allowing script 'unsafe-inline' so the UI is not black-screened", "error", err)
		return baseCSP("'unsafe-inline'")
	}
	hashes := inlineScriptHashes(html)
	if len(hashes) == 0 {
		slog.Warn("csp: no inline scripts found in index.html; allowing script 'unsafe-inline' as a fallback")
		return baseCSP("'unsafe-inline'")
	}
	return baseCSP(strings.Join(hashes, " "))
}

// baseCSP renders the policy with scriptExtra spliced into script-src.
func baseCSP(scriptExtra string) string {
	return "default-src 'self'; " +
		"script-src 'self' " + scriptExtra + "; " +
		"img-src 'self' data:; " +
		"style-src 'self' 'unsafe-inline'; " +
		"object-src 'none'; base-uri 'self'; frame-ancestors 'none'"
}

var inlineScriptRe = regexp.MustCompile(`(?is)<script(\s[^>]*)?>(.*?)</script>`)

// inlineScriptHashes returns a CSP source expression ('sha256-<base64>') for
// every INLINE <script> (no src attribute) in html. The digest is over the
// exact bytes between the tags, which is what a browser hashes.
func inlineScriptHashes(html []byte) []string {
	var out []string
	for _, m := range inlineScriptRe.FindAllSubmatch(html, -1) {
		attrs, body := m[1], m[2]
		if bytes.Contains(bytes.ToLower(attrs), []byte("src=")) {
			continue // external script: already allowed by 'self'
		}
		sum := sha256.Sum256(body)
		out = append(out, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return out
}

// rateLimitIngest caps event ingest per DSN public key (falling back to client
// IP when no key is present) so a captured DSN cannot flood the write path.
func (s *Server) rateLimitIngest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := ingestKey(r)
		bucket := "k:" + key
		if key == "" {
			bucket = "ip:" + clientIP(r)
		}
		if !s.ingestLimiter.Allow(bucket) {
			w.Header().Set("Retry-After", "60")
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the caller IP without the port. chi's RealIP middleware has
// already resolved X-Forwarded-For into RemoteAddr upstream.
func clientIP(r *http.Request) string {
	// Use SplitHostPort, not LastIndex(":"): a bare IPv6 literal (which realIP may store)
	// has many colons, and chopping at the last one corrupts it, mangling the rate-limit /
	// login-lockout bucket key.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// trustedProxyIP reports whether ip is an internal reverse-proxy address that
// we allow to set X-Forwarded-For (loopback, RFC1918/ULA private, link-local).
// Flare sits behind an internal nginx -> Caddy chain, so real client IPs arrive
// via X-Forwarded-For from a private/loopback peer.
func trustedProxyIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// realIP resolves the client IP from X-Forwarded-For ONLY when the immediate
// TCP peer is a trusted internal proxy. A directly-connected public client
// therefore cannot spoof X-Forwarded-For to rotate its apparent IP and evade
// the IP-keyed login lockout or ingest rate limit. It takes the right-most XFF
// entry that is not itself a trusted-proxy address (the real client as seen at
// our edge). This replaces chi's RealIP, which trusts the header unconditionally.
func realIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.RemoteAddr
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if peer := net.ParseIP(host); peer != nil && trustedProxyIP(peer) {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				parts := strings.Split(xff, ",")
				for i := len(parts) - 1; i >= 0; i-- {
					cand := strings.TrimSpace(parts[i])
					if ip := net.ParseIP(cand); ip != nil && !trustedProxyIP(ip) {
						r.RemoteAddr = cand
						break
					}
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireAuth accepts either a browser session (dashboard) or a Bearer API
// key (programmatic). It injects the user and org ids into the request
// context for downstream handlers.
// establishSession rotates the session token and records the authenticated
// user, org, and the auth epoch (login instant). The epoch lets requireAuth
// revoke sessions that predate a password reset (see users.sessions_valid_from).
// Every login path must go through here so the epoch is always set.
func (s *Server) establishSession(ctx context.Context, userID, orgID string) {
	if err := s.sessions.RenewToken(ctx); err != nil {
		return
	}
	s.sessions.Put(ctx, "user_id", userID)
	s.sessions.Put(ctx, "org_id", orgID)
	s.sessions.Put(ctx, "auth_epoch", time.Now().Unix())
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// An explicitly supplied credential is authoritative. A valid ambient
		// session must never replace an invalid, narrower or foreign bearer.
		if headers := r.Header.Values("Authorization"); len(headers) > 0 {
			key := bearerToken(r)
			if len(headers) != 1 || key == "" || len(key) > 256 {
				s.authenticationRequired(w)
				return
			}
			ak, err := s.q.GetAPIKeyByHash(ctx, auth.HashAPIKey(key))
			if err != nil || (ak.ExpiresAt.Valid && !ak.ExpiresAt.Time.After(time.Now())) {
				s.authenticationRequired(w)
				return
			}
			if ak.Purpose != "api" && !(ak.Purpose == "mcp" && r.URL.Path == mcpEndpoint) {
				writeErr(w, http.StatusForbidden, "credential is not valid for this API surface")
				return
			}
			// Fail closed even if a malformed stored MCP row bypassed the
			// database expiry constraint. Legacy API keys may remain unlimited.
			if ak.Purpose == "mcp" && !ak.ExpiresAt.Valid {
				s.authenticationRequired(w)
				return
			}
			_ = s.q.TouchAPIKey(ctx, ak.ID)
			ctx = context.WithValue(ctx, ctxUserID, "")
			ctx = context.WithValue(ctx, ctxOrgID, ak.OrgID)
			ctx = context.WithValue(ctx, ctxRole, apiKeyRole(ak.Role))
			ctx = context.WithValue(ctx, ctxAuthSource, "api_key")
			ctx = context.WithValue(ctx, ctxCredentialID, ak.ID)
			ctx = context.WithValue(ctx, ctxCredentialPurpose, ak.Purpose)
			if ak.ExpiresAt.Valid {
				ctx = context.WithValue(ctx, ctxCredentialExpiry, ak.ExpiresAt.Time)
			}
			s.recordUsage(ctx, usageDelta{
				OrgID: ak.OrgID, CredentialID: ak.ID,
				Surface: "api_key", Operation: apiKeyUsageOperation(r), Requests: 1,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		// Cookie-only browser sessions retain their independent CSRF gate.
		// Re-read the user so removal, role and password-reset changes apply.
		if user, err := s.nativeSessionUser(ctx); err == nil {
			ctx = context.WithValue(ctx, ctxUserID, user.ID)
			ctx = context.WithValue(ctx, ctxOrgID, user.OrgID)
			ctx = context.WithValue(ctx, ctxRole, user.Role)
			ctx = context.WithValue(ctx, ctxAuthSource, "session")
			ctx = context.WithValue(ctx, ctxCredentialPurpose, "session")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		} else if !errors.Is(err, errNativeSessionRequired) {
			writeErr(w, http.StatusServiceUnavailable, "session could not be checked")
			return
		}
		s.authenticationRequired(w)
	})
}

func (s *Server) authenticationRequired(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("WWW-Authenticate", `Bearer realm="flare"`)
	writeErr(w, http.StatusUnauthorized, "authentication required")
}

// requireRole returns middleware that rejects callers below min in the role
// hierarchy with 403.
// apiKeyRole resolves the authority a stored API key row actually gets.
//
// Only the two mintable key roles are honoured. Anything else, including a
// role written directly into the database, a value from a future migration, or
// an empty string on a row created before the column existed, collapses to
// read-only. A long-lived token must never be able to reach owner or admin,
// because those roles gate team membership, SSO and workspace deletion, so the
// safe direction on any surprise is down, not up.
func apiKeyRole(stored string) string {
	if stored == "viewer" || stored == "member" {
		return stored
	}
	return "viewer"
}

func (s *Server) requireRole(min string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !roleAtLeast(roleFrom(r.Context()), min) {
				// A known API key that reaches a role gate is a tenant-scoped,
				// attributable denial. Count it without persisting the bearer
				// value; MCP role refusals are accounted for inside mcpHandler,
				// where the JSON-RPC operation is known.
				if authSourceFrom(r.Context()) == "api_key" {
					s.recordUsage(r.Context(), usageDelta{
						OrgID: orgIDFrom(r.Context()), CredentialID: credentialIDFrom(r.Context()),
						Surface: "api_key", Operation: apiKeyUsageOperation(r), Denied: 1,
					})
				}
				writeErr(w, http.StatusForbidden, "insufficient permissions for this action")
				return
			}
			next.ServeHTTP(w, r.WithContext(r.Context()))
		})
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
