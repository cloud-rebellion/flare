package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/oidcprofile"
)

var oidcLaunchOrgPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// An empty selector leaves all organizations on their unchanged empty profile.
// A selected but incomplete profile affects that organization's SSO only.
func (s *Server) oidcAssurance(org string) (oidcprofile.Policy, error) {
	if s.cfg.OIDCLaunchOrgID == "" || org != s.cfg.OIDCLaunchOrgID {
		return oidcprofile.Policy{}, nil
	}
	if !oidcLaunchOrgPattern.MatchString(s.cfg.OIDCLaunchOrgID) {
		return oidcprofile.Policy{}, errors.New("invalid OIDC launch organization")
	}
	p, err := oidcprofile.Parse(s.cfg.OIDCRequiredACR, s.cfg.OIDCRequiredAMR)
	if err != nil || !p.Enabled() {
		return oidcprofile.Policy{}, errors.New("selected OIDC assurance profile incomplete")
	}
	return p, nil
}

func oidcBindingHash(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

// Only a selected active profile adds binding context. Unrelated organizations
// retain the original transaction hash byte-for-byte, even on the same issuer.
func (s *Server) oidcAssuranceBinding(org string, cfg *generated.OidcConfig, p oidcprofile.Policy) string {
	if !p.Enabled() {
		return ""
	}
	context, _ := json.Marshal(struct {
		Version, Org, Profile, ClientSecret, OperatorIssuer, Peer, DefaultRole string
		PrelinkedOnly, Enabled                                                 bool
	}{"flare-provider-assurance-v1", org, p.Digest(), oidcBindingHash(cfg.ClientSecret),
		oidcBindingHash(s.cfg.OIDCPrivateIssuer), s.cfg.OIDCPeerIP, cfg.DefaultRole, s.cfg.OIDCPrelinkedOnly, cfg.Enabled})
	return oidcBindingHash(string(context))
}
