package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/oidcprofile"
	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

func TestVerifiedOIDCAssuranceBeforeUserinfo(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := oidcprofile.Parse("urn:fixture:uv", "pop,mfa")
	if err != nil {
		t.Fatal(err)
	}
	issuer, client, nonce := "https://assurance-issuer.example", "assurance-client", strings.Repeat("n", 43)
	for _, name := range []string{
		"strong", "fractional-dates", "exponent-dates", "unicode-pair", "unicode-replacement", "quoted-backslash",
		"missing-acr", "wrong-acr", "duplicate-acr", "duplicate-amr", "duplicate-iat", "duplicate-iss",
		"missing-amr", "duplicate-method", "nonstring-method", "weak-method", "lone-surrogate", "invalid-utf8",
		"auth-time-string", "auth-time-fraction", "auth-time-exponent", "future-auth-time", "future-fractional-iat",
		"auth-after-iat", "iat-after-exp", "issuer", "audience", "nonce", "azp", "access-token-hash", "signature", "userinfo-subject",
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now().Unix()
			claims := map[string]any{"iss": issuer, "sub": "bound-subject", "aud": client, "nonce": nonce,
				"iat": now - 2, "exp": now + 120, "acr": profile.RequiredACR, "amr": []string{"extra", "mfa", "pop"}, "auth_time": now - 60}
			good := false
			switch name {
			case "strong", "unicode-pair", "unicode-replacement", "quoted-backslash":
				good = true
			case "fractional-dates":
				good = true
				claims["iat"], claims["exp"] = json.Number(fmt.Sprintf("%d.5", now-2)), json.Number(fmt.Sprintf("%d.5", now+120))
			case "exponent-dates":
				good = true
				claims["iat"], claims["exp"] = json.Number(fmt.Sprintf("%de0", now-2)), json.Number(fmt.Sprintf("%de0", now+120))
			case "missing-acr":
				delete(claims, "acr")
			case "wrong-acr":
				claims["acr"] = "urn:fixture:weak"
			case "missing-amr":
				delete(claims, "amr")
			case "duplicate-method":
				claims["amr"] = []string{"pop", "mfa", "pop"}
			case "nonstring-method":
				claims["amr"] = []any{"pop", "mfa", 1}
			case "weak-method":
				claims["amr"] = []string{"pwd", "otp"}
			case "auth-time-string":
				claims["auth_time"] = fmt.Sprint(now - 60)
			case "auth-time-fraction":
				claims["auth_time"] = json.Number(fmt.Sprintf("%d.0", now-60))
			case "auth-time-exponent":
				claims["auth_time"] = json.Number("1e3")
			case "future-auth-time":
				claims["auth_time"], claims["iat"] = now+60, now+60
			case "future-fractional-iat":
				claims["iat"] = json.Number(fmt.Sprintf("%d.5", now+30))
			case "auth-after-iat":
				claims["iat"] = now - 61
			case "iat-after-exp":
				claims["iat"] = now + 121
			case "issuer":
				claims["iss"] = "https://wrong.example"
			case "audience":
				claims["aud"] = "wrong-client"
			case "nonce":
				claims["nonce"] = "wrong"
			case "azp":
				claims["azp"] = "wrong-client"
			case "access-token-hash":
				claims["at_hash"] = "wrong"
			}
			if name == "unicode-pair" || name == "unicode-replacement" || name == "quoted-backslash" || name == "lone-surrogate" || name == "invalid-utf8" {
				claims["amr"] = []string{"pop", "mfa", "unicode-marker"}
			}
			payload, err := json.Marshal(claims)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "unicode-pair":
				payload = []byte(strings.Replace(string(payload), `"unicode-marker"`, `"\ud83d\ude00"`, 1))
			case "unicode-replacement":
				payload = []byte(strings.Replace(string(payload), `"unicode-marker"`, `"\ufffd"`, 1))
			case "quoted-backslash":
				payload = []byte(strings.Replace(string(payload), `"unicode-marker"`, `"\\ud800"`, 1))
			case "lone-surrogate":
				payload = []byte(strings.Replace(string(payload), `"unicode-marker"`, `"\ud800"`, 1))
			case "invalid-utf8":
				payload = []byte(strings.Replace(string(payload), `"unicode-marker"`, "\""+string([]byte{0xff})+"\"", 1))
			}
			if strings.HasPrefix(name, "duplicate-") && name != "duplicate-method" {
				field := strings.TrimPrefix(name, "duplicate-")
				value, _ := json.Marshal(claims[field])
				payload = []byte(strings.TrimSuffix(string(payload), "}") + ",\"" + field + "\":" + string(value) + "}")
			}
			signingKey := key
			if name == "signature" {
				signingKey, err = rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatal(err)
				}
			}
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: signingKey}, (&jose.SignerOptions{}).WithHeader("kid", "fixture"))
			if err != nil {
				t.Fatal(err)
			}
			signed, err := signer.Sign(payload)
			if err != nil {
				t.Fatal(err)
			}
			rawID, err := signed.CompactSerialize()
			if err != nil {
				t.Fatal(err)
			}
			jwks, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "fixture", Algorithm: "RS256", Use: "sig"}}})
			verifier := oauth2.GenerateVerifier()
			exchanges, userinfos := 0, 0
			provider := &Provider{Issuer: issuer, AuthURL: issuer + "/authorize", TokenURL: issuer + "/token", UserinfoURL: issuer + "/userinfo", JWKSURL: issuer + "/jwks"}
			provider.client = &http.Client{Transport: oidcRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/jwks":
					return oidcResponse(200, string(jwks)), nil
				case "/token":
					exchanges++
					raw, _ := io.ReadAll(r.Body)
					form, _ := url.ParseQuery(string(raw))
					if form.Get("code_verifier") != verifier || form.Get("client_secret") != "fixture-secret" || form.Get("redirect_uri") != "https://app.example/callback" {
						t.Error("token transaction binding changed")
					}
					body, _ := json.Marshal(map[string]any{"access_token": "fixture-access", "token_type": "Bearer", "id_token": rawID})
					return oidcResponse(200, string(body)), nil
				case "/userinfo":
					userinfos++
					subject := "bound-subject"
					if name == "userinfo-subject" {
						subject = "wrong-subject"
					}
					body, _ := json.Marshal(map[string]any{"sub": subject, "email": "fixture@example.test", "email_verified": true})
					return oidcResponse(200, string(body)), nil
				}
				t.Error("unexpected outbound request")
				return oidcResponse(404, ""), nil
			})}
			authURL, _ := url.Parse(provider.AuthorizationURLWithAssurance(client, "https://app.example/callback", "state", verifier, nonce, profile))
			if authURL.Query().Get("acr_values") != profile.RequiredACR || authURL.Query().Get("nonce") != nonce || authURL.Query().Get("code_challenge") != oauth2.S256ChallengeFromVerifier(verifier) {
				t.Fatal("assurance request lost configured proof")
			}
			for _, forbidden := range []string{"prompt", "max_age", "claims"} {
				if authURL.Query().Has(forbidden) {
					t.Fatal("forced freshness or essential claims added")
				}
			}
			info, err := provider.AuthenticateWithAssurance(context.Background(), client, "fixture-secret", "fixture-code", "https://app.example/callback", verifier, nonce, profile)
			if good && (err != nil || info == nil || info.Subject != "bound-subject") {
				t.Fatalf("strong signed evidence refused: %v", err)
			}
			if !good && (err == nil || info != nil) {
				t.Fatal("weak or invalid signed evidence accepted")
			}
			if exchanges != 1 || !good && name != "userinfo-subject" && userinfos != 0 {
				t.Fatal("weak login retried or reached userinfo before assurance")
			}
			if (good || name == "userinfo-subject") && userinfos != 1 {
				t.Fatal("verified identity did not use exactly one matching-subject userinfo check")
			}
		})
	}
}

func TestAssuranceMetadataDoesNotChangeGenericDiscovery(t *testing.T) {
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	issuer := "https://metadata-assurance.example"
	for _, optional := range []any{nil, "malformed", map[string]any{"unexpected": true}, []string{"urn:fixture:uv"}} {
		httpClient = &http.Client{Transport: oidcRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := json.Marshal(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "userinfo_endpoint": issuer + "/userinfo", "jwks_uri": issuer + "/jwks", "acr_values_supported": optional})
			return oidcResponse(200, string(body)), nil
		})}
		discMu.Lock()
		delete(discCache, issuer)
		discMu.Unlock()
		provider, err := Discover(context.Background(), issuer)
		if err != nil {
			t.Fatalf("optional assurance metadata changed generic discovery: %v", err)
		}
		_, advertised := optional.([]string)
		if provider.SupportsACR("urn:fixture:uv") != advertised {
			t.Fatal("selected readiness accepted an unadvertised class")
		}
		query, _ := url.Parse(provider.AuthorizationURL("client", "https://app.example/callback", "state", "verifier", "nonce"))
		if query.Query().Has("acr_values") {
			t.Fatal("ordinary provider acquired an assurance request")
		}
	}
}
