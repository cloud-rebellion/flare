package api

import (
	"net/http"
	"net/url"

	"github.com/bright-interaction/flare/internal/config"
)

// validMCPOrigin permits native clients with no Origin, and refuses untrusted
// browser origins before tool dispatch. This is not a CORS permission and never
// derives trusted authority from request Host or forwarded headers.
func (s *Server) validMCPOrigin(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 || len(s.cfg.MCPAllowedOrigins) > config.MaxMCPAllowedOrigins {
		return false
	}
	raw := values[0]
	if len(raw) > config.MaxMCPOriginConfigBytes {
		return false
	}
	allowedBytes := 0
	for i, allowed := range s.cfg.MCPAllowedOrigins {
		allowedBytes += len(allowed)
		if i > 0 {
			allowedBytes++
		}
		if allowedBytes > config.MaxMCPOriginConfigBytes {
			return false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Path != "" {
		return false
	}
	origin, err := config.NormalizeMCPOrigin(raw, s.cfg.IsProduction())
	if err != nil {
		return false
	}
	base, err := config.NormalizeMCPOrigin(s.cfg.BaseURL, s.cfg.IsProduction())
	if err != nil {
		return false
	}
	if origin == base {
		return true
	}
	for _, allowed := range s.cfg.MCPAllowedOrigins {
		candidate, err := config.NormalizeMCPOrigin(allowed, s.cfg.IsProduction())
		if err == nil && origin == candidate {
			return true
		}
	}
	return false
}
