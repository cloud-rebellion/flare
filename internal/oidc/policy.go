package oidc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// CloudRebellionIssuer is the sole private issuer this operator exception supports.
// Empty operator settings preserve the normal public-destination dial guard.
const CloudRebellionIssuer = "https://cloudrebellion.tech"
const maxOIDCResponse = 256 << 10

func ValidateOperatorPolicy(issuer, peer string) error {
	if issuer == "" && peer == "" {
		return nil
	}
	ip := net.ParseIP(peer)
	if issuer != CloudRebellionIssuer || ip == nil || ip.String() != peer ||
		!ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return errors.New("invalid operator OIDC issuer or exact peer pin")
	}
	return nil
}

type boundedOIDCTransport struct {
	base http.RoundTripper
	pin  bool
}

func (t boundedOIDCTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.pin && !pinnedProtocolRequest(r) {
		return nil, errors.New("OIDC pinned endpoint refused")
	}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, errors.New("OIDC request unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxOIDCResponse+1))
	if err != nil || len(body) > maxOIDCResponse {
		return nil, errors.New("OIDC response exceeds bounds")
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}

func pinnedProtocolRequest(r *http.Request) bool {
	if r.URL.Scheme != "https" || r.URL.Host != "cloudrebellion.tech" || r.URL.User != nil ||
		r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.Fragment != "" {
		return false
	}
	switch r.URL.Path {
	case "/.well-known/openid-configuration", "/.well-known/jwks.json", "/api/oidc/userinfo":
		return r.Method == http.MethodGet
	case "/api/oidc/token":
		return r.Method == http.MethodPost
	}
	return false
}

func providerHTTPClient(issuer, operatorIssuer, peer string) (*http.Client, error) {
	if err := ValidateOperatorPolicy(operatorIssuer, peer); err != nil {
		return nil, err
	}
	base := httpClient.Transport
	if base == nil {
		base = publicOIDCTransport()
	}
	pinned := operatorIssuer != "" && issuer == operatorIssuer
	if pinned {
		ip := net.ParseIP(peer)
		dialer := &net.Dialer{Timeout: 5 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				host, port, err := net.SplitHostPort(address)
				if err != nil || port != "443" || !ip.Equal(net.ParseIP(host)) {
					return errors.New("OIDC peer differs from exact operator pin")
				}
				return nil
			}}
		base = &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "cloudrebellion.tech:443" {
				return nil, errors.New("OIDC pinned authority refused")
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), "443"))
		}}
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: boundedOIDCTransport{base: base, pin: pinned},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("OIDC redirects disabled") }}, nil
}

func validIssuer(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil &&
		u.RawQuery == "" && u.Fragment == ""
}
