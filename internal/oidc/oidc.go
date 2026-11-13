// Package oidc is a bounded OpenID Connect relying-party client. Tokens are
// verified with coreos/go-oidc; access tokens are never persisted.
package oidc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bright-interaction/flare/internal/netguard"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/metrics"
)

// httpClient is SSRF-guarded: the Control hook runs after DNS resolution with
// the dialed IP, so an admin-supplied issuer cannot reach loopback/private/
// link-local addresses (cloud metadata, internal services) even under DNS
// rebinding. Same pattern as internal/alerts.
var httpClient = &http.Client{
	Timeout:   10 * time.Second,
	Transport: publicOIDCTransport(),
	// No redirects. The token request carries client_secret in its body, and a
	// discovery document that redirects is indistinguishable from one that
	// moves the credential somewhere else. sameAuthority pins the discovered
	// endpoints to the issuer host; a redirect would step outside that check
	// after it had already passed.
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("redirects disabled")
	},
}

func publicOIDCTransport() http.RoundTripper {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				if netguard.IsBlockedIP(net.ParseIP(host)) {
					return errors.New("blocked destination address")
				}
				return nil
			},
		}).DialContext,
	}
}

// sameAuthority reports whether endpoint is https and its host equals the
// issuer host or is a subdomain of it. Binds discovered endpoints to the
// configured (trusted) issuer so a malicious/compromised discovery document
// cannot redirect the code + client_secret to an attacker host.
func sameAuthority(issuerHost, endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" {
		return false
	}
	h := u.Hostname()
	return h == issuerHost || strings.HasSuffix(h, "."+issuerHost)
}

// Provider holds the endpoints resolved from an issuer's discovery document.
type Provider struct {
	Issuer      string `json:"issuer"`
	AuthURL     string `json:"authorization_endpoint"`
	TokenURL    string `json:"token_endpoint"`
	UserinfoURL string `json:"userinfo_endpoint"`
	JWKSURL     string `json:"jwks_uri"`
	// Optional assurance metadata is decoded only for selected launch readiness.
	// Its shape must not change discovery for ordinary unrelated providers.
	ACRValues json.RawMessage `json:"acr_values_supported,omitempty"`
	client    *http.Client
}

var (
	discMu    sync.Mutex
	discCache = map[string]*Provider{}
)

// Discover fetches (and caches) the issuer's OIDC discovery document.
func Discover(ctx context.Context, issuer string) (provider *Provider, err error) {
	return DiscoverWithPolicy(ctx, issuer, "", "")
}

// DiscoverWithPolicy only allows a private peer for the exact operator-owned
// canonical CloudRebellion issuer. Tenant configuration cannot enable it.
func DiscoverWithPolicy(ctx context.Context, issuer, operatorIssuer, peer string) (provider *Provider, err error) {
	issuer = strings.TrimRight(issuer, "/")
	if !validIssuer(issuer) || netguard.ValidatePublicURL(issuer) != nil {
		return nil, errors.New("OIDC issuer refused")
	}
	client, err := providerHTTPClient(issuer, operatorIssuer, peer)
	if err != nil {
		return nil, err
	}
	cacheKey := issuer
	if operatorIssuer != "" {
		cacheKey += "\x00" + operatorIssuer + "\x00" + peer
	}
	discMu.Lock()
	if p, ok := discCache[cacheKey]; ok {
		discMu.Unlock()
		return p, nil
	}
	discMu.Unlock()

	startedAt := time.Now().UTC()
	started := startedAt
	defer func() {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		metrics.RecordIntegration("oidc", "discover", "http", outcome, time.Since(started))
		receiptOutcome, statusClass, errorCode := integration.ClassifyError(err)
		receiptErr := integration.RecordResult(ctx, integration.Attempt{
			Scope: integration.ScopeFrom(ctx), Provider: "oidc", Operation: "discover", Channel: "http",
			Outcome: receiptOutcome, StatusClass: statusClass, ErrorCode: errorCode,
			Elapsed: time.Since(started), AttemptedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
		if receiptErr != nil {
			slog.Warn("connector receipt not accepted", "provider", "oidc", "operation", "discover", "receipt_status", integration.ReceiptStatus(receiptErr))
		}
	}()
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc discovery returned %d", resp.StatusCode)
	}
	var p Provider
	if err := decodeOIDCJSON(resp.Body, &p); err != nil {
		return nil, err
	}
	if p.Issuer != issuer || p.AuthURL == "" || p.TokenURL == "" || p.UserinfoURL == "" || p.JWKSURL == "" {
		return nil, fmt.Errorf("oidc discovery missing endpoints")
	}
	// Bind every endpoint to the issuer's authority (https + same host or a
	// subdomain). Rejects a discovery doc that points the token/userinfo
	// endpoints (which receive the code + client_secret) at a foreign host.
	issuerHost := ""
	if iu, err := url.Parse(issuer); err == nil {
		issuerHost = iu.Hostname()
	}
	if issuerHost == "" ||
		!sameAuthority(issuerHost, p.AuthURL) ||
		!sameAuthority(issuerHost, p.TokenURL) ||
		!sameAuthority(issuerHost, p.UserinfoURL) || !sameAuthority(issuerHost, p.JWKSURL) {
		return nil, fmt.Errorf("oidc discovery endpoints are not under the issuer authority")
	}
	discMu.Lock()
	p.client = client
	discCache[cacheKey] = &p
	discMu.Unlock()
	return &p, nil
}

// AuthCodeURL builds the authorization request URL.
func (p *Provider) AuthCodeURL(clientID, redirectURI, state string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	return p.AuthURL + "?" + q.Encode()
}

// Exchange swaps an authorization code for an access token (client_secret_post).
func (p *Provider) Exchange(ctx context.Context, clientID, clientSecret, code, redirectURI string) (accessToken string, err error) {
	startedAt := time.Now().UTC()
	started := startedAt
	defer func() {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		metrics.RecordIntegration("oidc", "exchange", "http", outcome, time.Since(started))
		receiptOutcome, statusClass, errorCode := integration.ClassifyError(err)
		receiptErr := integration.RecordResult(ctx, integration.Attempt{
			Scope: integration.ScopeFrom(ctx), Provider: "oidc", Operation: "exchange", Channel: "http",
			Outcome: receiptOutcome, StatusClass: statusClass, ErrorCode: errorCode,
			Elapsed: time.Since(started), AttemptedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
		if receiptErr != nil {
			slog.Warn("connector receipt not accepted", "provider", "oidc", "operation", "exchange", "receipt_status", integration.ReceiptStatus(receiptErr))
		}
	}()
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("client_id", clientID)
	data.Set("client_secret", clientSecret)
	data.Set("code", code)
	data.Set("redirect_uri", redirectURI)

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token exchange returned %d", resp.StatusCode)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	if err := decodeOIDCJSON(resp.Body, &tr); err != nil || tr.AccessToken == "" {
		return "", fmt.Errorf("token exchange: no access_token")
	}
	return tr.AccessToken, nil
}

// UserInfo is the subset of the OIDC userinfo response we use.
type UserInfo struct {
	// Subject is the IdP's stable, immutable identifier for the user ("sub").
	// Accounts are bound to it, because an email can be reassigned at the IdP
	// (or simply asserted by whoever controls the IdP config).
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

// FetchUserInfo reads the userinfo endpoint with the access token.
func (p *Provider) FetchUserInfo(ctx context.Context, accessToken string) (info *UserInfo, err error) {
	startedAt := time.Now().UTC()
	started := startedAt
	defer func() {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		metrics.RecordIntegration("oidc", "fetch_userinfo", "http", outcome, time.Since(started))
		receiptOutcome, statusClass, errorCode := integration.ClassifyError(err)
		receiptErr := integration.RecordResult(ctx, integration.Attempt{
			Scope: integration.ScopeFrom(ctx), Provider: "oidc", Operation: "fetch_userinfo", Channel: "http",
			Outcome: receiptOutcome, StatusClass: statusClass, ErrorCode: errorCode,
			Elapsed: time.Since(started), AttemptedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
		if receiptErr != nil {
			slog.Warn("connector receipt not accepted", "provider", "oidc", "operation", "fetch_userinfo", "receipt_status", integration.ReceiptStatus(receiptErr))
		}
	}()
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.UserinfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := p.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo returned %d", resp.StatusCode)
	}
	// email_verified may arrive as a bool or a JSON string; decode loosely.
	var raw map[string]any
	if err := decodeOIDCJSON(resp.Body, &raw); err != nil {
		return nil, err
	}
	info = &UserInfo{}
	if v, ok := raw["sub"].(string); ok {
		info.Subject = v
	}
	if v, ok := raw["email"].(string); ok {
		info.Email = v
	}
	if v, ok := raw["name"].(string); ok {
		info.Name = v
	}
	switch v := raw["email_verified"].(type) {
	case bool:
		info.EmailVerified = v
	case string:
		info.EmailVerified = v == "true"
	}
	return info, nil
}

func (p *Provider) http() *http.Client {
	if p.client != nil {
		return p.client
	}
	client, _ := providerHTTPClient("", "", "")
	return client
}

func decodeOIDCJSON(r io.Reader, target any) error {
	data, err := io.ReadAll(io.LimitReader(r, maxOIDCResponse+1))
	if err != nil || len(data) > maxOIDCResponse || json.Unmarshal(data, target) != nil {
		return errors.New("OIDC response invalid")
	}
	return nil
}

// RandomState returns a URL-safe random state token.
func RandomState() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
