package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/config"
)

func TestMCPOriginAllowsOnlyConfiguredBrowserAuthorities(t *testing.T) {
	s := &Server{cfg: config.Config{Environment: "production", BaseURL: "https://flare.example/", MCPAllowedOrigins: []string{"https://private.example:8443"}}}
	for _, tc := range []struct {
		name    string
		origins []string
		allowed bool
	}{
		{"native client", nil, true},
		{"canonical", []string{"https://flare.example"}, true},
		{"canonical case and default port", []string{"https://FLARE.example:443"}, true},
		{"explicit alias", []string{"https://private.example:8443"}, true},
		{"foreign", []string{"https://evil.example"}, false},
		{"suffix confusion", []string{"https://flare.example.evil.example"}, false},
		{"wrong scheme", []string{"http://flare.example"}, false},
		{"wrong port", []string{"https://private.example"}, false},
		{"opaque origin", []string{"null"}, false},
		{"empty header", []string{""}, false},
		{"duplicate", []string{"https://flare.example", "https://flare.example"}, false},
		{"mixed duplicate", []string{"https://evil.example", "https://flare.example"}, false},
		{"combined origins", []string{"https://flare.example https://evil.example"}, false},
		{"combined header", []string{"https://flare.example,https://evil.example"}, false},
		{"userinfo", []string{"https://user:secret@flare.example"}, false},
		{"path", []string{"https://flare.example/path"}, false},
		{"root path in header", []string{"https://flare.example/"}, false},
		{"empty query", []string{"https://flare.example?"}, false},
		{"empty fragment", []string{"https://flare.example#"}, false},
		{"oversized", []string{strings.Repeat("x", config.MaxMCPOriginConfigBytes+1)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "https://evil.example/api/mcp", nil)
			// Neither attacker-controlled authority source can add a trusted origin.
			r.Host = "evil.example"
			r.Header.Set("X-Forwarded-Host", "evil.example")
			r.Header.Set("X-Forwarded-Proto", "https")
			for _, value := range tc.origins {
				r.Header.Add("Origin", value)
			}
			if got := s.validMCPOrigin(r); got != tc.allowed {
				t.Fatalf("Origin allowed=%v, want %v", got, tc.allowed)
			}
		})
	}
}

func TestMCPOriginInvalidBaseFailsBrowserRequestsClosed(t *testing.T) {
	for _, base := range []string{"", "https://user:secret@flare.example", "https://flare.example/path", "https://flare.example?", "https://flare.example#", "http://flare.example"} {
		s := &Server{cfg: config.Config{Environment: "production", BaseURL: base, MCPAllowedOrigins: []string{"https://private.example"}}}
		r := httptest.NewRequest(http.MethodPost, "/api/mcp", nil)
		if !s.validMCPOrigin(r) {
			t.Errorf("native client refused with base %q", base)
		}
		r.Header.Set("Origin", "https://private.example")
		if s.validMCPOrigin(r) {
			t.Errorf("browser accepted with invalid base %q", base)
		}
	}
}
