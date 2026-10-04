package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/secretbox"
)

type oauthRoundTrip func(*http.Request) (*http.Response, error)

func (f oauthRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func oauthResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestOAuthAuthorizeExchangeRefreshRevoke(t *testing.T) {
	var calls []string
	client := &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.RawQuery != "" {
			t.Error("OAuth secret leaked into URL")
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("wrong content type")
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 16<<10))
		v, _ := url.ParseQuery(string(raw))
		if v.Get("client_id") != "client-id" || v.Get("client_secret") != "client-secret" {
			t.Error("client credentials missing from form body")
		}
		calls = append(calls, r.URL.Path+":"+v.Get("grant_type"))
		switch r.URL.Path {
		case oauthTokenPath:
			if v.Get("grant_type") == "authorization_code" && (v.Get("code") != "one-use-code" || v.Get("redirect_uri") != "https://flare.example/api/connectors/hubspot/callback") {
				t.Error("wrong code exchange")
			}
			if v.Get("grant_type") == "refresh_token" && v.Get("refresh_token") != "refresh-1" {
				t.Error("wrong refresh token")
			}
			body, _ := json.Marshal(OAuthTokens{AccessToken: "access-2", RefreshToken: "refresh-2", TokenType: "bearer", HubID: 1234, Scopes: []string{"oauth"}, ExpiresIn: 1800})
			return oauthResponse(http.StatusOK, string(body)), nil
		case oauthRevokePath:
			if v.Get("token") != "refresh-2" || v.Get("token_type_hint") != "refresh_token" {
				t.Error("wrong revoke request")
			}
			return oauthResponse(http.StatusNoContent, ""), nil
		default:
			t.Error("unexpected OAuth path")
		}
		return oauthResponse(http.StatusNotFound, ""), nil
	})}
	c := OAuthClient{ClientID: "client-id", ClientSecret: "client-secret", RedirectURI: "https://flare.example/api/connectors/hubspot/callback", HTTPClient: client}
	authURL, err := c.AuthorizationURL("state-value")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	if u.Scheme != "https" || u.Host != "app.hubspot.com" || u.Query().Get("scope") != "oauth" || u.Query().Get("state") != "state-value" || u.Query().Get("client_secret") != "" {
		t.Fatal("unsafe authorization URL")
	}
	if _, err := c.Exchange(context.Background(), "one-use-code"); err != nil {
		t.Fatal(err)
	}
	tokens, err := c.Refresh(context.Background(), "refresh-1")
	if err != nil || tokens.HubID != 1234 || tokens.RefreshToken != "refresh-2" {
		t.Fatalf("refresh = %#v, %v", tokens, err)
	}
	if err := c.Revoke(context.Background(), tokens.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != oauthTokenPath+":authorization_code,"+oauthTokenPath+":refresh_token,"+oauthRevokePath+":" {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestOAuthRejectsBadResponseWithoutProviderLeak(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{400, `{"error":"provider-secret"}`},
		{200, `{"access_token":"access","refresh_token":"refresh","token_type":"bearer","hub_id":123,"scopes":["crm.objects.contacts.read"],"expires_in":1800}`},
		{200, `{"access_token":"access","refresh_token":"refresh","token_type":"bearer","scopes":["oauth"],"expires_in":1800}`},
	} {
		client := &http.Client{Transport: oauthRoundTrip(func(_ *http.Request) (*http.Response, error) { return oauthResponse(tc.status, tc.body), nil })}
		c := OAuthClient{ClientID: "id", ClientSecret: "secret", RedirectURI: "https://flare.example/callback", HTTPClient: client}
		_, err := c.Exchange(context.Background(), "code")
		if err == nil || strings.Contains(err.Error(), "provider-secret") || strings.Contains(err.Error(), "access") || strings.Contains(err.Error(), "refresh") {
			t.Fatalf("unsafe OAuth error: %v", err)
		}
	}
}

func TestOAuthRefreshFailureUsesOnlyFixedHealthCodes(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantCode, wantStatus string
		status                           int
	}{
		{"revoked grant", `{"error":"invalid_grant","error_description":"private provider detail"}`, "auth_expired", "4xx", http.StatusBadRequest},
		{"bad client", `{"error":"invalid_client","error_description":"private provider detail"}`, "invalid_config", "4xx", http.StatusBadRequest},
		{"unknown bad request", `{"error":"private provider detail"}`, "invalid_config", "4xx", http.StatusBadRequest},
		{"rate limit", `{"error":"private provider detail"}`, "rate_limited", "4xx", http.StatusTooManyRequests},
		{"provider outage", `{"error":"private provider detail"}`, "provider_error", "5xx", http.StatusServiceUnavailable},
		{"malformed success", `{"access_token":"private provider detail"}`, "invalid_response", "2xx", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: oauthRoundTrip(func(_ *http.Request) (*http.Response, error) {
				return oauthResponse(tc.status, tc.body), nil
			})}
			oauth := OAuthClient{ClientID: "id", ClientSecret: "secret", RedirectURI: "https://flare.example/callback", HTTPClient: client}
			_, err := oauth.Refresh(context.Background(), "refresh-token")
			code, statusClass, observed := PreflightFailureDetails(err)
			if !observed || code != tc.wantCode || statusClass != tc.wantStatus {
				t.Fatalf("refresh classification = %q/%q/%v, err=%v", code, statusClass, observed, err)
			}
			if strings.Contains(err.Error(), "private provider detail") || strings.Contains(err.Error(), "refresh-token") {
				t.Fatalf("refresh error leaked provider data: %v", err)
			}
		})
	}
}

func TestOAuthRefreshCanceledRequestIsNotProviderHealth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	oauth := OAuthClient{ClientID: "id", ClientSecret: "secret", RedirectURI: "https://flare.example/callback"}
	_, err := oauth.Refresh(ctx, "refresh-token")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh = %v", err)
	}
	if code, _, observed := PreflightFailureDetails(err); observed {
		t.Fatalf("canceled request reported provider health code %q", code)
	}
}

func TestOAuthCredentialEncryptionAndManualToken(t *testing.T) {
	cred := NewOAuthCredential("org-1", "project-1", "conn-1", OAuthTokens{AccessToken: "access-token", RefreshToken: "refresh-token", HubID: 123, ExpiresIn: 1800}, time.Unix(1000, 0))
	if stored, err := cred.Encode(secretbox.New("")); err == nil || stored != "" {
		t.Fatal("plaintext OAuth write accepted")
	}
	cipher := secretbox.New("unit-test-secret")
	stored, err := cred.Encode(cipher)
	if err != nil || !strings.HasPrefix(stored, "enc:v1:") || strings.Contains(stored, "refresh-token") {
		t.Fatalf("unsafe OAuth storage: %v", err)
	}
	decoded, manual, oauth, err := DecodeOAuthCredential(cipher, stored)
	if err != nil || !oauth || manual != "" || decoded.ProjectID != "project-1" || decoded.RefreshToken != "refresh-token" {
		t.Fatalf("OAuth decode = %#v/%q/%v/%v", decoded, manual, oauth, err)
	}
	_, manual, oauth, err = DecodeOAuthCredential(cipher, cipher.Encrypt("manual-token"))
	if err != nil || oauth || manual != "manual-token" {
		t.Fatalf("manual decode = %q/%v/%v", manual, oauth, err)
	}
}
