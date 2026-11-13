package config

import "testing"

func TestOIDCAssuranceDefaultsAndInvalidSelectionKeepNativeStartup(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://fixture/unused")
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("FLARE_OIDC_PRIVATE_ISSUER", "")
	t.Setenv("FLARE_OIDC_PEER_IP", "")
	t.Setenv("FLARE_OIDC_PRELINKED_ONLY", "false")
	for _, field := range []string{"FLARE_OIDC_REQUIRED_ACR", "FLARE_OIDC_REQUIRED_AMR", "FLARE_OIDC_LAUNCH_ORG_ID"} {
		t.Setenv(field, "")
	}
	cfg, err := Load()
	if err != nil || cfg.OIDCRequiredACR != "" || cfg.OIDCRequiredAMR != "" || cfg.OIDCLaunchOrgID != "" {
		t.Fatal("assurance or portal launch enabled by default")
	}
	t.Setenv("FLARE_OIDC_REQUIRED_ACR", "urn:fixture:uv")
	t.Setenv("FLARE_OIDC_LAUNCH_ORG_ID", "bad org")
	cfg, err = Load()
	if err != nil || cfg.OIDCRequiredACR != "urn:fixture:uv" || cfg.OIDCLaunchOrgID != "bad org" {
		t.Fatal("invalid optional selection blocked native application startup")
	}
}

func TestOIDCOperatorPolicyDefaultsAndExplicitPilot(t *testing.T) {
	for _, tc := range []struct {
		name, issuer, peer, prelinked string
		valid                         bool
	}{
		{"defaults", "", "", "false", true},
		{"pilot", "https://cloudrebellion.tech", "100.64.0.10", "true", true},
		{"flag without pin", "", "", "true", false},
		{"wrong issuer", "https://other.example", "100.64.0.10", "true", false},
		{"cidr", "https://cloudrebellion.tech", "100.64.0.10/32", "false", false},
		{"missing peer", "https://cloudrebellion.tech", "", "false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://fixture/unused")
			t.Setenv("ENVIRONMENT", "development")
			t.Setenv("FLARE_OIDC_PRIVATE_ISSUER", tc.issuer)
			t.Setenv("FLARE_OIDC_PEER_IP", tc.peer)
			t.Setenv("FLARE_OIDC_PRELINKED_ONLY", tc.prelinked)
			cfg, err := Load()
			if (err == nil) != tc.valid {
				t.Fatalf("operator policy validity mismatch: %v", err)
			}
			if tc.name == "defaults" && (cfg.OIDCPrivateIssuer != "" || cfg.OIDCPeerIP != "" || cfg.OIDCPrelinkedOnly) {
				t.Fatal("pilot enabled by default")
			}
		})
	}
}
