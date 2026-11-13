package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/config"
)

func txServer() *Server {
	return &Server{cfg: config.Config{SecretKey: "test-secret-key-for-oidc-tx-cookie", Environment: "production"}}
}

// carry moves the cookie the response set onto a fresh request, the way a
// browser would on the IdP's redirect back.
func carry(t *testing.T, rec *httptest.ResponseRecorder) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback", nil)
	for _, c := range rec.Result().Cookies() {
		r.AddCookie(c)
	}
	return r
}

func txProof() oidcTransaction {
	return oidcTransaction{State: strings.Repeat("s", 43), Org: "org-123", Issuer: "https://issuer.example", ClientID: "client", RedirectURI: "https://flare.example/api/auth/oidc/callback", Nonce: strings.Repeat("n", 43), Verifier: strings.Repeat("v", 43), Expires: time.Now().Add(oidcTxTTL).Unix()}
}

func TestOIDCTxRoundTrip(t *testing.T) {
	s := txServer()
	rec := httptest.NewRecorder()
	proof := txProof()
	s.setOIDCTx(rec, proof)

	got, ok := s.readOIDCTx(carry(t, rec))
	if !ok || got != proof {
		t.Fatal("signed transaction fields did not round trip")
	}
}

func TestOIDCTxSelectedAssuranceContextRoundTripsAndRejectsMalformedBinding(t *testing.T) {
	s := txServer()
	proof := txProof()
	proof.Assurance = strings.Repeat("a", 64)
	w := httptest.NewRecorder()
	s.setOIDCTx(w, proof)
	if got, ok := s.readOIDCTx(carry(t, w)); !ok || got != proof {
		t.Fatal("selected assurance binding did not survive the signed one-use proof")
	}
	for _, invalid := range []string{"short", strings.Repeat("A", 64), strings.Repeat("a", 65)} {
		proof.Assurance = invalid
		w = httptest.NewRecorder()
		s.setOIDCTx(w, proof)
		if _, ok := s.readOIDCTx(carry(t, w)); ok {
			t.Fatal("malformed assurance binding accepted")
		}
	}
}

// The whole reason this cookie exists: the session cookie is SameSite=Strict,
// so the browser withholds it on the IdP's cross-site redirect back and SSO
// could never complete. Lax is sent on exactly that navigation.
func TestOIDCTxCookieAttributes(t *testing.T) {
	s := txServer()
	rec := httptest.NewRecorder()
	s.setOIDCTx(rec, txProof())

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected exactly one cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax (Strict is withheld on the IdP return)", c.SameSite)
	}
	if !c.HttpOnly {
		t.Error("cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Error("cookie must be Secure in production")
	}
	if c.MaxAge <= 0 || c.MaxAge > 900 {
		t.Errorf("MaxAge = %d, want a short positive TTL", c.MaxAge)
	}
}

func TestOIDCTxRejectsTampering(t *testing.T) {
	s := txServer()
	rec := httptest.NewRecorder()
	s.setOIDCTx(rec, txProof())
	original := rec.Result().Cookies()[0].Value

	tests := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"garbage", "not-a-cookie"},
		{"too few parts", "a.b.c"},
		{"truncated mac", original[:len(original)-4]},
		// Forge a different org with a signature that cannot match.
		{"swapped org", "c3RhdGUtYWJj.b3RoZXItb3Jn.99999999999.AAAA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback", nil)
			r.AddCookie(&http.Cookie{Name: oidcTxCookie, Value: tt.value})
			if _, ok := s.readOIDCTx(r); ok {
				t.Fatal("tampered cookie accepted")
			}
		})
	}
}

// A cookie signed with a different key must never validate.
func TestOIDCTxRejectsForeignKey(t *testing.T) {
	rec := httptest.NewRecorder()
	txServer().setOIDCTx(rec, txProof())

	other := &Server{cfg: config.Config{SecretKey: "a-completely-different-secret-key"}}
	if _, ok := other.readOIDCTx(carry(t, rec)); ok {
		t.Fatal("cookie from another key accepted")
	}
}

func TestOIDCTxClearExpiresTheCookie(t *testing.T) {
	s := txServer()
	rec := httptest.NewRecorder()
	s.clearOIDCTx(rec)
	c := rec.Result().Cookies()[0]
	if c.MaxAge >= 0 {
		t.Fatalf("MaxAge = %d, want negative so the browser drops it", c.MaxAge)
	}
	if c.Value != "" {
		t.Fatalf("Value = %q, want empty", c.Value)
	}
}

func TestOIDCTxRejectsExpiredOrIncompleteProof(t *testing.T) {
	for _, tc := range []string{"expired", "missing nonce", "missing verifier", "future"} {
		t.Run(tc, func(t *testing.T) {
			s := txServer()
			proof := txProof()
			switch tc {
			case "expired":
				proof.Expires = time.Now().Add(-time.Second).Unix()
			case "missing nonce":
				proof.Nonce = ""
			case "missing verifier":
				proof.Verifier = ""
			case "future":
				proof.Expires = time.Now().Add(2 * oidcTxTTL).Unix()
			}
			response := httptest.NewRecorder()
			s.setOIDCTx(response, proof)
			if _, ok := s.readOIDCTx(carry(t, response)); ok {
				t.Fatal("invalid signed transaction accepted")
			}
		})
	}
}
