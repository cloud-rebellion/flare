package config

import (
	"strings"
	"testing"
)

func TestMCPAllowedOriginsLoadBoundsAndNormalizesExplicitAliases(t *testing.T) {
	prodEnv(t, "")
	t.Setenv("FLARE_MCP_ALLOWED_ORIGINS", " https://FLARE.example:443/ ,https://private.example:8443 ")
	c, err := Load()
	if err != nil || len(c.MCPAllowedOrigins) != 2 || c.MCPAllowedOrigins[0] != "https://flare.example" || c.MCPAllowedOrigins[1] != "https://private.example:8443" {
		t.Fatalf("explicit aliases: %v, %v", c.MCPAllowedOrigins, err)
	}
	for _, raw := range []string{
		strings.Repeat("x", MaxMCPOriginConfigBytes+1),
		strings.Repeat(" ", MaxMCPOriginConfigBytes+1),
		strings.Repeat("https://flare.example,", MaxMCPAllowedOrigins) + "https://last.example",
		"https://flare.example,", "https://flare.example,,https://other.example",
		"https://flare.example,https://FLARE.example:443", "*", "null",
		"https://*.example", "https://user:secret@flare.example",
		"https://flare.example/path", "https://flare.example?", "https://flare.example#",
		"https://flare.example:0", "https://flare.example:65536", "https://flare.example:",
		"http://localhost:8080", "http://127.0.0.1:8080", "https://flare.example\nhttps://other.example",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("FLARE_MCP_ALLOWED_ORIGINS", raw)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "FLARE_MCP_ALLOWED_ORIGINS") || strings.Contains(err.Error(), "user:secret") {
				t.Fatalf("invalid or unsafe origin config accepted: %v", err)
			}
		})
	}
	t.Setenv("FLARE_MCP_ALLOWED_ORIGINS", "")
	if c, err := Load(); err != nil || len(c.MCPAllowedOrigins) != 0 {
		t.Fatalf("optional aliases: %v, %v", c.MCPAllowedOrigins, err)
	}
}

func TestMCPOriginDevelopmentLoopbackAndDistinctAuthorities(t *testing.T) {
	for _, raw := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080", "https://flare.example"} {
		if _, err := NormalizeMCPOrigin(raw, false); err != nil {
			t.Errorf("development origin %q refused: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://flare.example", "http://192.168.1.10", "http://localhost.evil.example", "https://[flare.example]", "https://[127.0.0.1]", "https://[fe80::1%25en0]", "https://flare.example/%2f", "https://flare.example//"} {
		if _, err := NormalizeMCPOrigin(raw, false); err == nil {
			t.Errorf("invalid development origin %q accepted", raw)
		}
	}
	v4, err := NormalizeMCPOrigin("https://127.0.0.1", true)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := NormalizeMCPOrigin("https://[::ffff:127.0.0.1]", true)
	if err != nil || mapped == v4 {
		t.Fatalf("distinct browser authorities collapsed: %q %q %v", v4, mapped, err)
	}
}
