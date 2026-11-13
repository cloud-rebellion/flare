package api

import (
	"crypto/sha256"
	"testing"

	"github.com/bright-interaction/flare/internal/config"
	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestOIDCAssuranceSelectsOnlyExactOrganizationAndPreservesLegacyHash(t *testing.T) {
	s := &Server{cfg: config.Config{OIDCLaunchOrgID: "selected-org", OIDCRequiredACR: "urn:fixture:uv", OIDCRequiredAMR: "pop,mfa"}}
	cfg := &generated.OidcConfig{Issuer: "https://issuer.example", ClientID: "client", ClientSecret: "sealed-fixture", Enabled: true, DefaultRole: "member"}
	for _, org := range []string{"unrelated-org", "selected-org-other", ""} {
		for _, broken := range []bool{false, true} {
			if broken {
				s.cfg.OIDCRequiredAMR = "pop,,mfa"
			}
			p, err := s.oidcAssurance(org)
			if err != nil || p.Enabled() || s.oidcAssuranceBinding(org, cfg, p) != "" {
				t.Fatal("selected profile changed an unrelated organization")
			}
			proof := oidcTransaction{Org: org, Issuer: cfg.Issuer, ClientID: cfg.ClientID, RedirectURI: "https://app.example/api/auth/oidc/callback"}
			legacy := sha256.Sum256([]byte(proof.Issuer + "\x00" + proof.ClientID + "\x00" + proof.RedirectURI))
			if proof.configHash() != legacy {
				t.Fatal("empty profile changed the legacy transaction bytes")
			}
		}
	}
	s.cfg.OIDCRequiredAMR = "pop,mfa"
	p, err := s.oidcAssurance("selected-org")
	if err != nil || !p.Enabled() {
		t.Fatal("complete selected profile refused")
	}
	first := s.oidcAssuranceBinding("selected-org", cfg, p)
	if len(first) != 64 {
		t.Fatal("selected profile has no complete binding")
	}
	proof := oidcTransaction{Org: "selected-org", Issuer: cfg.Issuer, ClientID: cfg.ClientID, RedirectURI: "https://app.example/api/auth/oidc/callback", Assurance: first}
	withProfile := proof.configHash()
	proof.Assurance = ""
	if proof.configHash() == withProfile {
		t.Fatal("enabled and missing-context flows share a transaction binding")
	}
	cfg.ClientSecret = "new-sealed-fixture"
	if s.oidcAssuranceBinding("selected-org", cfg, p) == first {
		t.Fatal("selected client-secret change retained pending authority")
	}
}

func TestOIDCAssuranceIncompleteSelectedProfileDoesNotAffectOthers(t *testing.T) {
	for _, tc := range []struct{ org, acr, amr string }{
		{"selected", "", ""}, {"selected", "urn:fixture:uv", ""},
		{"selected", "", "pop"}, {"selected", "urn:fixture:uv", "pop,pop"},
		{"bad org", "urn:fixture:uv", "pop,mfa"},
	} {
		s := &Server{cfg: config.Config{OIDCLaunchOrgID: tc.org, OIDCRequiredACR: tc.acr, OIDCRequiredAMR: tc.amr}}
		if _, err := s.oidcAssurance(tc.org); err == nil {
			t.Fatal("selected invalid profile became ready")
		}
		if p, err := s.oidcAssurance("unrelated"); err != nil || p.Enabled() {
			t.Fatal("invalid selected profile affected an unrelated workspace")
		}
	}
	s := &Server{cfg: config.Config{OIDCRequiredACR: "unused", OIDCRequiredAMR: "invalid,,unused"}}
	if p, err := s.oidcAssurance("any-org"); err != nil || p.Enabled() {
		t.Fatal("empty selector did not retain the generic default")
	}
}
