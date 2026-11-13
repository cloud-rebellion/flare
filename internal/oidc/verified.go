package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/metrics"
	"github.com/bright-interaction/flare/internal/oidcprofile"
	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// AuthorizationURL binds the browser authorization to fresh PKCE and nonce
// values retained only in its signed, HttpOnly transaction cookie.
func (p *Provider) AuthorizationURL(clientID, redirectURI, state, verifier, nonce string) string {
	return p.AuthorizationURLWithAssurance(clientID, redirectURI, state, verifier, nonce, oidcprofile.Policy{})
}

func (p *Provider) AuthorizationURLWithAssurance(clientID, redirectURI, state, verifier, nonce string, profile oidcprofile.Policy) string {
	c := oauth2.Config{ClientID: clientID, RedirectURL: redirectURI,
		Endpoint: oauth2.Endpoint{AuthURL: p.AuthURL, TokenURL: p.TokenURL},
		Scopes:   []string{coreoidc.ScopeOpenID, "email", "profile"}}
	options := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier), coreoidc.Nonce(nonce)}
	if profile.Enabled() {
		options = append(options, oauth2.SetAuthURLParam("acr_values", profile.RequiredACR))
	}
	return c.AuthCodeURL(state, options...)
}

// Authenticate never uses userinfo as an identity assertion by itself. A
// verified ID token and an equal userinfo subject are both required.
func (p *Provider) Authenticate(ctx context.Context, clientID, secret, code, redirectURI, verifier, nonce string) (*UserInfo, error) {
	return p.AuthenticateWithAssurance(ctx, clientID, secret, code, redirectURI, verifier, nonce, oidcprofile.Policy{})
}

func (p *Provider) AuthenticateWithAssurance(ctx context.Context, clientID, secret, code, redirectURI, verifier, nonce string, profile oidcprofile.Policy) (*UserInfo, error) {
	access, subject, err := p.exchangeVerifiedWithAssurance(ctx, clientID, secret, code, redirectURI, verifier, nonce, profile)
	if err != nil {
		return nil, err
	}
	info, err := p.FetchUserInfo(ctx, access)
	if err != nil || info == nil || info.Subject != subject {
		return nil, errors.New("OIDC userinfo identity refused")
	}
	return info, nil
}

func (p *Provider) exchangeVerifiedWithAssurance(ctx context.Context, clientID, secret, code, redirectURI, verifier, nonce string, profile oidcprofile.Policy) (access, subject string, err error) {
	started := time.Now().UTC()
	defer func() {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		metrics.RecordIntegration("oidc", "exchange", "http", outcome, time.Since(started))
		o, status, code := integration.ClassifyError(err)
		receiptErr := integration.RecordResult(ctx, integration.Attempt{
			Scope: integration.ScopeFrom(ctx), Provider: "oidc", Operation: "exchange", Channel: "http",
			Outcome: o, StatusClass: status, ErrorCode: code, Elapsed: time.Since(started),
			AttemptedAt: started, CompletedAt: time.Now().UTC(),
		})
		if receiptErr != nil {
			slog.Warn("connector receipt not accepted", "provider", "oidc", "operation", "exchange", "receipt_status", integration.ReceiptStatus(receiptErr))
		}
	}()
	if p.Issuer == "" || p.JWKSURL == "" || clientID == "" || len(clientID) > 256 || code == "" || len(code) > 4096 ||
		len(secret) > 65536 || len(redirectURI) > 2048 || len(verifier) < 43 || len(verifier) > 128 || nonce == "" || len(nonce) > 128 {
		return "", "", errors.New("OIDC transaction incomplete")
	}
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	requestCtx = coreoidc.ClientContext(requestCtx, p.http())
	c := oauth2.Config{ClientID: clientID, ClientSecret: secret, RedirectURL: redirectURI,
		Endpoint: oauth2.Endpoint{TokenURL: p.TokenURL, AuthStyle: oauth2.AuthStyleInParams}}
	token, exchangeErr := c.Exchange(requestCtx, code, oauth2.VerifierOption(verifier))
	if exchangeErr != nil || token == nil || token.AccessToken == "" || !strings.EqualFold(token.TokenType, "Bearer") {
		return "", "", errors.New("OIDC token exchange refused")
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return "", "", errors.New("OIDC ID token required")
	}
	keys := coreoidc.NewRemoteKeySet(requestCtx, p.JWKSURL)
	v := coreoidc.NewVerifier(p.Issuer, keys, &coreoidc.Config{ClientID: clientID,
		SupportedSigningAlgs: []string{coreoidc.RS256, coreoidc.RS384, coreoidc.RS512,
			coreoidc.ES256, coreoidc.ES384, coreoidc.ES512, coreoidc.PS256, coreoidc.PS384, coreoidc.PS512, coreoidc.EdDSA}})
	idToken, verifyErr := v.Verify(requestCtx, raw)
	if verifyErr != nil || idToken.Issuer != p.Issuer || idToken.Subject == "" || len(idToken.Subject) > 255 ||
		subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return "", "", errors.New("OIDC ID token refused")
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
	}
	if idToken.Claims(&claims) != nil || (len(idToken.Audience) > 1 && claims.AuthorizedParty != clientID) ||
		(claims.AuthorizedParty != "" && claims.AuthorizedParty != clientID) {
		return "", "", errors.New("OIDC authorized party refused")
	}
	if idToken.AccessTokenHash != "" && idToken.VerifyAccessToken(token.AccessToken) != nil {
		return "", "", errors.New("OIDC access token binding refused")
	}
	if profile.Enabled() {
		var verified json.RawMessage
		if idToken.Claims(&verified) != nil || profile.Validate(verified, clientID, idToken.Audience, time.Now().Unix()) != nil {
			return "", "", errors.New("OIDC provider assurance refused")
		}
	}
	return token.AccessToken, idToken.Subject, nil
}

func (p *Provider) SupportsACR(acr string) bool {
	var values []string
	if len(p.ACRValues) > 65536 || json.Unmarshal(p.ACRValues, &values) != nil || len(values) > 64 {
		return false
	}
	for _, value := range values {
		if value == acr {
			return true
		}
	}
	return false
}
