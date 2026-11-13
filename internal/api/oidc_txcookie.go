package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	oidcTxCookie = "flare_oidc_tx"
	oidcTxPath   = "/api/auth/oidc"
	oidcTxTTL    = 10 * time.Minute
)

var oidcProofPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
var oidcAssurancePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Server) oidcTxMAC(payload string) []byte {
	m := hmac.New(sha256.New, []byte(s.cfg.SecretKey))
	m.Write([]byte("flare-oidc-transaction-v2\x00"))
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// The session remains Strict. Only this short-lived callback cookie is Lax.
// Atomic server consumption, rather than cookie deletion, enforces one-use.
func (s *Server) setOIDCTx(w http.ResponseWriter, proof oidcTransaction) {
	raw, _ := json.Marshal(proof)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{Name: oidcTxCookie, Value: payload + "." + base64.RawURLEncoding.EncodeToString(s.oidcTxMAC(payload)),
		Path: oidcTxPath, MaxAge: int(oidcTxTTL.Seconds()), HttpOnly: true, Secure: s.cfg.IsProduction(), SameSite: http.SameSiteLaxMode})
}

func (s *Server) clearOIDCTx(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: oidcTxCookie, Value: "", Path: oidcTxPath, MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.IsProduction(), SameSite: http.SameSiteLaxMode})
}

func (s *Server) readOIDCTx(r *http.Request) (oidcTransaction, bool) {
	var proof oidcTransaction
	c, err := r.Cookie(oidcTxCookie)
	if err != nil || len(c.Value) > 4096 || s.cfg.SecretKey == "" {
		return proof, false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 2 {
		return proof, false
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(mac, s.oidcTxMAC(parts[0])) {
		return proof, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(raw, &proof) != nil || proof.Org == "" || len(proof.Org) > 128 ||
		proof.Issuer == "" || len(proof.Issuer) > 2048 || proof.ClientID == "" || len(proof.ClientID) > 256 ||
		!oidcProofPattern.MatchString(proof.State) || !oidcProofPattern.MatchString(proof.Nonce) ||
		!oidcProofPattern.MatchString(proof.Verifier) || proof.RedirectURI == "" ||
		(proof.Assurance != "" && !oidcAssurancePattern.MatchString(proof.Assurance)) ||
		proof.Expires <= time.Now().Unix() || proof.Expires > time.Now().Add(oidcTxTTL).Unix() {
		return oidcTransaction{}, false
	}
	return proof, true
}
