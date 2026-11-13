package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

func TestVerifiedOIDCProtocol(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer, client, nonce, verifier := "https://verified-issuer.example", "fixture-client", strings.Repeat("n", 43), oauth2.GenerateVerifier()
	for _, tc := range []string{"valid", "missing id token", "issuer", "audience", "expiry", "nonce", "azp", "signature", "userinfo subject", "unsigned", "missing email", "pkce", "missing nonce", "multi audience without azp", "multi audience valid azp", "access token hash"} {
		t.Run(tc, func(t *testing.T) {
			claims := map[string]any{"iss": issuer, "sub": "stable-subject", "aud": client, "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(), "nonce": nonce}
			switch tc {
			case "issuer":
				claims["iss"] = "https://other.example"
			case "audience":
				claims["aud"] = "other-client"
			case "expiry":
				claims["exp"] = time.Now().Add(-time.Minute).Unix()
			case "nonce":
				claims["nonce"] = "wrong"
			case "azp":
				claims["azp"] = "other-client"
			case "missing nonce":
				delete(claims, "nonce")
			case "multi audience without azp":
				claims["aud"] = []string{client, "other-client"}
			case "multi audience valid azp":
				claims["aud"] = []string{client, "other-client"}
				claims["azp"] = client
			case "access token hash":
				claims["at_hash"] = "wrong"
			}
			signerKey := key
			if tc == "signature" {
				signerKey = other
			}
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: signerKey}, (&jose.SignerOptions{}).WithHeader("kid", "fixture"))
			if err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(claims)
			signed, err := signer.Sign(payload)
			if err != nil {
				t.Fatal(err)
			}
			rawID, err := signed.CompactSerialize()
			if err != nil {
				t.Fatal(err)
			}
			if tc == "missing id token" {
				rawID = ""
			}
			if tc == "unsigned" {
				rawID = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
			}
			previous := httpClient
			t.Cleanup(func() { httpClient = previous })
			discovery := map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "userinfo_endpoint": issuer + "/userinfo", "jwks_uri": issuer + "/jwks"}
			jwks, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "fixture", Algorithm: "RS256", Use: "sig"}}})
			exchanges := 0
			httpClient = &http.Client{Transport: oidcRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				var body []byte
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					body, _ = json.Marshal(discovery)
				case "/jwks":
					body = jwks
				case "/token":
					exchanges++
					b, _ := io.ReadAll(r.Body)
					form, _ := url.ParseQuery(string(b))
					if tc == "pkce" && form.Get("code_verifier") != verifier {
						return oidcResponse(400, `{"error":"invalid_grant"}`), nil
					}
					if form.Get("code_verifier") != verifier || form.Get("client_secret") != "fixture-secret" || r.URL.RawQuery != "" {
						t.Error("token credentials or PKCE not confined to expected form")
					}
					body, _ = json.Marshal(map[string]any{"access_token": "fixture-access", "token_type": "Bearer", "id_token": rawID})
				case "/userinfo":
					if r.Header.Get("Authorization") != "Bearer fixture-access" {
						t.Error("userinfo credential missing")
					}
					sub := "stable-subject"
					if tc == "userinfo subject" {
						sub = "other-subject"
					}
					info := map[string]any{"sub": sub, "email": "fixture@example.test", "email_verified": true}
					if tc == "missing email" {
						delete(info, "email")
						info["email_verified"] = false
					}
					body, _ = json.Marshal(info)
				default:
					t.Error("unexpected outbound path")
					return oidcResponse(404, ""), nil
				}
				return oidcResponse(200, string(body)), nil
			})}
			discMu.Lock()
			delete(discCache, issuer)
			discMu.Unlock()
			provider, err := Discover(context.Background(), issuer)
			if err != nil {
				t.Fatal(err)
			}
			authURL, _ := url.Parse(provider.AuthorizationURL(client, "https://flare.example/callback", "state", verifier, nonce))
			if authURL.Query().Get("code_challenge") != oauth2.S256ChallengeFromVerifier(verifier) || authURL.Query().Get("code_challenge_method") != "S256" || authURL.Query().Get("nonce") != nonce {
				t.Fatal("authorization proof missing")
			}
			usedVerifier := verifier
			if tc == "pkce" {
				usedVerifier = strings.Repeat("x", 43)
			}
			info, err := provider.Authenticate(context.Background(), client, "fixture-secret", "fixture-code", "https://flare.example/callback", usedVerifier, nonce)
			good := tc == "valid" || tc == "missing email" || tc == "multi audience valid azp"
			if good && (err != nil || info == nil || info.Subject != "stable-subject") {
				t.Fatalf("valid protocol refused: %v", err)
			}
			if !good && err == nil {
				t.Fatal("invalid protocol accepted")
			}
			if err != nil && (strings.Contains(err.Error(), "fixture-access") || strings.Contains(err.Error(), rawID) && rawID != "") {
				t.Fatal("provider token leaked in error")
			}
			if exchanges != 1 {
				t.Fatalf("exchange was retried: %d", exchanges)
			}
		})
	}
}

func TestOIDCOperatorPinIsExactAndOptIn(t *testing.T) {
	for _, tc := range []struct {
		issuer, peer string
		valid        bool
	}{
		{"", "", true}, {CloudRebellionIssuer, "100.64.0.10", true},
		{CloudRebellionIssuer + "/", "100.64.0.10", false}, {"https://other.example", "100.64.0.10", false},
		{CloudRebellionIssuer, "100.64.0.10/32", false}, {CloudRebellionIssuer, "127.0.0.1", false},
		{CloudRebellionIssuer, "169.254.169.254", false}, {CloudRebellionIssuer, "", false}, {"", "100.64.0.10", false},
	} {
		if (ValidateOperatorPolicy(tc.issuer, tc.peer) == nil) != tc.valid {
			t.Error("operator pin validation mismatch")
		}
	}
	for _, raw := range []string{"https://cloudrebellion.tech:444/api/oidc/token", "https://evil.cloudrebellion.tech/api/oidc/token", "https://cloudrebellion.tech/api/oidc/token?extra=1", "https://cloudrebellion.tech/admin", "https://user@cloudrebellion.tech/api/oidc/token"} {
		req, _ := http.NewRequest(http.MethodPost, raw, nil)
		if pinnedProtocolRequest(req) {
			t.Error("unexpected pinned endpoint accepted")
		}
	}
	req, _ := http.NewRequest(http.MethodPost, CloudRebellionIssuer+"/api/oidc/token", nil)
	if !pinnedProtocolRequest(req) {
		t.Fatal("exact pinned token endpoint refused")
	}
}

func TestOIDCResponseLimitAndProviderErrorRedaction(t *testing.T) {
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	httpClient = &http.Client{Transport: oidcRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return oidcResponse(200, strings.Repeat("x", maxOIDCResponse+1)), nil
	})}
	client, err := providerHTTPClient("https://issuer.example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://issuer.example/document", nil)
	if _, err = client.Do(request); err == nil {
		t.Fatal("oversized provider response accepted")
	}
	httpClient = &http.Client{Transport: oidcRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return oidcResponse(400, `{"error":"private-code-and-token"}`), nil
	})}
	provider := &Provider{TokenURL: "https://issuer.example/token"}
	_, err = provider.Exchange(context.Background(), "client", "secret", "code", "https://flare.example/callback")
	if err == nil || strings.Contains(err.Error(), "private-code-and-token") {
		t.Fatal("provider error body exposed")
	}
}

func TestVerifiedDiscoveryRejectsIssuerOrJWKSDrift(t *testing.T) {
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	issuer := "https://strict-discovery.example"
	for _, tc := range []string{"issuer", "missing jwks", "foreign jwks", "userinfo endpoint"} {
		t.Run(tc, func(t *testing.T) {
			document := map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "userinfo_endpoint": issuer + "/userinfo", "jwks_uri": issuer + "/jwks"}
			switch tc {
			case "issuer":
				document["issuer"] = "https://different.example"
			case "missing jwks":
				delete(document, "jwks_uri")
			case "foreign jwks":
				document["jwks_uri"] = "https://other.example/keys"
			case "userinfo endpoint":
				document["userinfo_endpoint"] = "https://user@strict-discovery.example/userinfo"
			}
			raw, _ := json.Marshal(document)
			httpClient = &http.Client{Transport: oidcRoundTripFunc(func(*http.Request) (*http.Response, error) { return oidcResponse(200, string(raw)), nil })}
			discMu.Lock()
			delete(discCache, issuer)
			discMu.Unlock()
			if _, err := Discover(context.Background(), issuer); err == nil {
				t.Fatal("unsafe discovery accepted")
			}
		})
	}
}

func TestGenericPublicIssuerPreservesCustomPortAndEscapedRealm(t *testing.T) {
	const issuer = "https://realm-issuer.example:8443/realms/team%2Fspace"
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	document := map[string]any{
		"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
		"token_endpoint": issuer + "/token", "userinfo_endpoint": issuer + "/userinfo",
		"jwks_uri": issuer + "/jwks",
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	httpClient = &http.Client{Transport: oidcRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != issuer+"/.well-known/openid-configuration" {
			t.Fatal("generic issuer port or escaped realm changed")
		}
		return oidcResponse(200, string(raw)), nil
	})}
	discMu.Lock()
	delete(discCache, issuer)
	discMu.Unlock()
	t.Cleanup(func() {
		discMu.Lock()
		delete(discCache, issuer)
		discMu.Unlock()
	})
	provider, err := Discover(context.Background(), issuer)
	if err != nil || provider == nil || provider.Issuer != issuer || calls != 1 {
		t.Fatalf("previously supported public issuer refused: %v", err)
	}
	for _, endpoint := range []string{provider.AuthURL, provider.TokenURL, provider.UserinfoURL, provider.JWKSURL} {
		u, err := url.Parse(endpoint)
		if err != nil || u.Port() != "8443" || !strings.HasPrefix(u.EscapedPath(), "/realms/team%2Fspace/") {
			t.Fatal("discovered endpoint port or escaped realm changed")
		}
		r, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil || pinnedProtocolRequest(r) {
			t.Fatal("generic endpoint entered the private issuer exception")
		}
	}
}
