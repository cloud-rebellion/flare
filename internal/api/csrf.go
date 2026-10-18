package api

import (
	"net/http"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/gorilla/csrf"
)

// conditionalCSRF enforces CSRF on cookie-authenticated (browser) requests and
// skips it for pure Bearer API clients, which carry no ambient credential and
// are therefore immune to CSRF by construction.
//
// The exemption used to be "an Authorization header is present", full stop, and
// that is not the same question. Historically requireAuth read the SESSION
// first and only fell through to the API key when there was no session, so the
// exemption decision and the authentication decision were made from two
// different inputs and never reconciled: `curl -H 'Authorization: Bearer x'`
// with a valid session cookie was CSRF-exempt AND session-authenticated. Junk
// in the header was enough; the value was never looked at.
//
// It was blocked twice over in practice (SameSite=Strict on the cookie, and a
// custom header forces a CORS preflight Flare answers with no
// Access-Control-Allow-Origin), which is exactly the problem: the CSRF layer
// was contributing nothing, and the whole defence rested on two controls one
// config change away, a proxy that relaxes SameSite or a future CORS middleware
// for a separate frontend origin.
//
// So the test is now "is there an ambient credential on this request", which is
// the thing CSRF is actually about. A session cookie means enforce, whatever
// else is present. This runs before requireAuth, so it reads the cookie rather
// than the resolved session. REST keeps this conservative gate because the same
// wrapper also protects login/logout routes outside requireAuth. The dedicated
// MCP transport instead uses authoritative bearer authentication before any
// cookie fallback, allowing an explicit bearer without browser CSRF.
func conditionalCSRF(csrfMW func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		protected := csrfMW(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if bearerToken(r) != "" && !hasSessionCookie(r) {
				next.ServeHTTP(w, r)
				return
			}
			protected.ServeHTTP(w, r)
		})
	}
}

// hasSessionCookie reports whether the request carries the browser's ambient
// session credential.
func hasSessionCookie(r *http.Request) bool {
	c, err := r.Cookie(auth.SessionCookieName)
	return err == nil && c.Value != ""
}

// handleCSRFToken returns the per-session CSRF token the SPA echoes back in
// the X-CSRF-Token header on state-changing requests.
func (s *Server) handleCSRFToken(w http.ResponseWriter, r *http.Request) {
	token := ""
	if !(s.cfg.DisableCSRF && !s.cfg.IsProduction()) {
		token = csrf.Token(r)
	}
	writeJSON(w, http.StatusOK, map[string]string{"csrf_token": token})
}
