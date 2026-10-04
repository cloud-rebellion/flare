package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	oauthAuthorizeURL  = "https://app.hubspot.com/oauth/authorize"
	oauthAPIURL        = "https://api.hubapi.com"
	oauthTokenPath     = "/oauth/2026-03/token"
	oauthRevokePath    = "/oauth/2026-03/token/revoke"
	oauthTimeout       = 12 * time.Second
	oauthResponseLimit = 32 << 10
)

// OAuthClient is for a single operator-registered confidential HubSpot app.
// Production authorities are fixed; test-only fields allow a local HTTP server.
// Neither provider error bodies nor credential values are returned in errors.
type OAuthClient struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	APIURL       string
	HTTPClient   *http.Client
}

type OAuthTokens struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	TokenType    string   `json:"token_type"`
	HubID        int64    `json:"hub_id"`
	Scopes       []string `json:"scopes"`
	ExpiresIn    int64    `json:"expires_in"`
}

// PreflightFailure is a fixed, payload-free outcome for a credential or token
// refresh failure that happened before the account-details probe. Database
// failures are not represented by this type, so callers can keep them as
// infrastructure errors instead of reporting a false provider observation.
type PreflightFailure struct {
	code        string
	statusClass string
}

func (f *PreflightFailure) Error() string { return "hubspot credential preflight failed" }

func preflightFailure(code, statusClass string) error {
	return &PreflightFailure{code: code, statusClass: statusClass}
}

func PreflightFailureDetails(err error) (code, statusClass string, ok bool) {
	var failure *PreflightFailure
	if !errors.As(err, &failure) {
		return "", "", false
	}
	return failure.code, failure.statusClass, true
}

func (c OAuthClient) Configured() bool {
	u, err := url.Parse(c.RedirectURI)
	return err == nil && u.Scheme != "" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" &&
		c.ClientID != "" && c.ClientSecret != "" && len(c.ClientID) <= 1024 && len(c.ClientSecret) <= 1024 &&
		!strings.ContainsAny(c.ClientID+c.ClientSecret, "\r\n")
}

func (c OAuthClient) AuthorizationURL(state string) (string, error) {
	if !c.Configured() || state == "" {
		return "", errors.New("hubspot oauth is unavailable")
	}
	u, _ := url.Parse(oauthAuthorizeURL)
	q := u.Query()
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", c.RedirectURI)
	// /account-info/v3/details needs only oauth. Broader CRM data scopes are
	// deliberately outside this health-only installation.
	q.Set("scope", "oauth")
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c OAuthClient) Exchange(ctx context.Context, code string) (OAuthTokens, error) {
	if code == "" || len(code) > 2048 || strings.ContainsAny(code, "\r\n") {
		return OAuthTokens{}, errors.New("hubspot authorization code is invalid")
	}
	return c.token(ctx, url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {c.RedirectURI},
	})
}

func (c OAuthClient) Refresh(ctx context.Context, refreshToken string) (OAuthTokens, error) {
	if refreshToken == "" || len(refreshToken) > 4096 || strings.ContainsAny(refreshToken, "\r\n") {
		return OAuthTokens{}, preflightFailure("invalid_config", "")
	}
	return c.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
}

func (c OAuthClient) token(ctx context.Context, form url.Values) (OAuthTokens, error) {
	if !c.Configured() {
		return OAuthTokens{}, preflightFailure("invalid_config", "")
	}
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)
	response, err := c.postForm(ctx, oauthTokenPath, form)
	if err != nil {
		if ctx.Err() != nil {
			return OAuthTokens{}, ctx.Err()
		}
		return OAuthTokens{}, preflightFailure("provider_error", "")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		code := "provider_error"
		switch response.StatusCode {
		case http.StatusBadRequest:
			// Only the standard OAuth invalid_grant code proves the stored
			// refresh grant is rejected. Never retain error_description or a
			// provider response body in a receipt or public error.
			var providerError struct {
				Error string `json:"error"`
			}
			if form.Get("grant_type") == "refresh_token" {
				_ = json.NewDecoder(io.LimitReader(response.Body, oauthResponseLimit)).Decode(&providerError)
				if providerError.Error == "invalid_grant" {
					code = "auth_expired"
				} else {
					code = "invalid_config"
				}
			}
		case http.StatusUnauthorized, http.StatusForbidden:
			code = "invalid_config"
		case http.StatusTooManyRequests:
			code = "rate_limited"
		}
		return OAuthTokens{}, preflightFailure(code, statusClass(response.StatusCode))
	}
	var tokens OAuthTokens
	if err := json.NewDecoder(io.LimitReader(response.Body, oauthResponseLimit)).Decode(&tokens); err != nil {
		return OAuthTokens{}, preflightFailure("invalid_response", "2xx")
	}
	if tokens.TokenType != "bearer" && tokens.TokenType != "Bearer" ||
		tokens.AccessToken == "" || tokens.RefreshToken == "" ||
		len(tokens.AccessToken) > 4096 || len(tokens.RefreshToken) > 4096 ||
		strings.ContainsAny(tokens.AccessToken+tokens.RefreshToken, "\r\n") ||
		tokens.HubID <= 0 || tokens.ExpiresIn <= 0 || tokens.ExpiresIn > 3600 ||
		len(tokens.Scopes) > 64 {
		return OAuthTokens{}, preflightFailure("invalid_response", "2xx")
	}
	foundOAuth := false
	for _, scope := range tokens.Scopes {
		if len(scope) > 128 {
			return OAuthTokens{}, preflightFailure("invalid_response", "2xx")
		}
		if scope == "oauth" {
			foundOAuth = true
		}
	}
	if !foundOAuth {
		return OAuthTokens{}, preflightFailure("invalid_response", "2xx")
	}
	return tokens, nil
}

// Revoke removes the long-lived grant. A failure keeps the local connection
// disabled for retry; callers must not silently report a successful removal.
func (c OAuthClient) Revoke(ctx context.Context, refreshToken string) error {
	if !c.Configured() || refreshToken == "" || len(refreshToken) > 4096 {
		return errors.New("hubspot revocation is unavailable")
	}
	response, err := c.postForm(ctx, oauthRevokePath, url.Values{
		"client_id": {c.ClientID}, "client_secret": {c.ClientSecret},
		"token": {refreshToken}, "token_type_hint": {"refresh_token"},
	})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	// RFC 7009 revocation is idempotent; HubSpot returns 2xx for success.
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("hubspot token revocation failed: " + strconv.Itoa(response.StatusCode/100) + "xx")
	}
	return nil
}

func (c OAuthClient) postForm(ctx context.Context, path string, form url.Values) (*http.Response, error) {
	base := c.APIURL
	if base == "" {
		base = oauthAPIURL
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: oauthTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	requestCtx, cancel := context.WithTimeout(ctx, oauthTimeout)
	// The response body must be read before the request context is canceled.
	// Keep the cancel attached to the body for all success and error paths.
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, base+path, strings.NewReader(form.Encode()))
	if err != nil {
		cancel()
		return nil, errors.New("hubspot oauth request is invalid")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, errors.New("hubspot oauth request failed")
	}
	resp.Body = &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *cancelReadCloser) Close() error { err := r.ReadCloser.Close(); r.cancel(); return err }
