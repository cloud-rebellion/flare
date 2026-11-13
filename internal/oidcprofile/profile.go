// Package oidcprofile validates an explicitly configured signed OIDC assurance
// profile. An empty policy preserves ordinary provider behavior.
package oidcprofile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Policy struct {
	RequiredACR string   `json:"acr"`
	RequiredAMR []string `json:"amr"`
}

func token(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) || unicode.IsSpace(c) {
			return false
		}
	}
	return true
}

// Parse accepts either an entirely empty policy or a complete bounded pair.
func Parse(acr, methods string) (Policy, error) {
	if acr == "" && methods == "" {
		return Policy{}, nil
	}
	p := Policy{RequiredACR: acr}
	if !token(acr, 256) {
		return Policy{}, errors.New("invalid required OIDC ACR")
	}
	seen := map[string]bool{}
	for _, method := range strings.Split(methods, ",") {
		method = strings.TrimSpace(method)
		if !token(method, 64) || seen[method] || len(p.RequiredAMR) >= 16 {
			return Policy{}, errors.New("invalid required OIDC AMR")
		}
		seen[method] = true
		p.RequiredAMR = append(p.RequiredAMR, method)
	}
	sort.Strings(p.RequiredAMR)
	return p, nil
}

func (p Policy) Enabled() bool { return p.RequiredACR != "" }

// Empty policies have the unchanged empty binding used by unrelated flows.
func (p Policy) Digest() string {
	if !p.Enabled() {
		return ""
	}
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

var critical = map[string]bool{
	"iss": true, "sub": true, "aud": true, "iat": true, "exp": true,
	"nbf": true, "nonce": true, "azp": true, "at_hash": true,
	"acr": true, "amr": true, "auth_time": true,
}

// Object rejects duplicate critical claims instead of depending on JSON's
// last-value-wins interpretation. Only cryptographically verified input belongs here.
func Object(raw []byte) (map[string]json.RawMessage, error) {
	if len(raw) > 256<<10 {
		return nil, errors.New("OIDC claims exceed bounds")
	}
	if !validJSONStrings(raw) {
		return nil, errors.New("invalid OIDC Unicode string")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, errors.New("invalid OIDC claims")
	}
	out := make(map[string]json.RawMessage)
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			return nil, errors.New("invalid OIDC claim key")
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, errors.New("invalid OIDC claim value")
		}
		if critical[name] {
			if _, exists := out[name]; exists {
				return nil, errors.New("duplicate OIDC claim")
			}
			out[name] = value
		}
	}
	if t, err = d.Token(); err != nil || t != json.Delim('}') {
		return nil, errors.New("invalid OIDC claims end")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("trailing OIDC claims")
	}
	return out, nil
}

// encoding/json otherwise repairs lone UTF16 surrogates to U+FFFD. Scan raw
// string escapes once before decoding; quoted backslashes are not Unicode escapes.
// The ordinary decoder remains responsible for the complete JSON grammar.
func validJSONStrings(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	inString := false
	for i := 0; i < len(raw); i++ {
		if !inString {
			if raw[i] == '"' {
				inString = true
			}
			continue
		}
		if raw[i] == '"' {
			inString = false
			continue
		}
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		unit, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if unit >= 0xdc00 && unit <= 0xdfff {
			return false
		}
		if unit >= 0xd800 && unit <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return !inString
}

func positiveInteger(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || raw[0] < '1' || raw[0] > '9' {
		return 0, errors.New("invalid OIDC integer time")
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid OIDC integer time")
		}
	}
	v, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || v <= 0 {
		return 0, errors.New("invalid OIDC integer time")
	}
	return v, nil
}

// Signed iat/exp retain NumericDate fractions/exponents, compared exactly.
// Bound both lexical length and exponent before big.Rat can allocate.
func positiveDate(raw json.RawMessage) (*big.Rat, error) {
	if len(raw) == 0 || len(raw) > 128 || raw[0] < '0' || raw[0] > '9' {
		return nil, errors.New("invalid OIDC NumericDate")
	}
	if at := strings.IndexAny(string(raw), "eE"); at >= 0 {
		exponent, err := strconv.ParseInt(string(raw[at+1:]), 10, 32)
		if err != nil || exponent < -1024 || exponent > 1024 {
			return nil, errors.New("OIDC NumericDate exponent exceeds bounds")
		}
	}
	v, ok := new(big.Rat).SetString(string(raw))
	if !ok || v.Sign() <= 0 {
		return nil, errors.New("invalid OIDC NumericDate")
	}
	return v, nil
}

// Validate checks only the signed assurance contract, after ordinary signature,
// issuer, audience, expiry and nonce verification. It never supplies a local time.
func (p Policy) Validate(raw []byte, clientID string, audience []string, now int64) error {
	if !p.Enabled() {
		return nil
	}
	claims, err := Object(raw)
	if err != nil {
		return err
	}
	var acr string
	if json.Unmarshal(claims["acr"], &acr) != nil || acr != p.RequiredACR {
		return errors.New("OIDC assurance class refused")
	}
	var amr []string
	if json.Unmarshal(claims["amr"], &amr) != nil || len(amr) == 0 || len(amr) > 16 {
		return errors.New("OIDC methods refused")
	}
	seen := map[string]bool{}
	for _, method := range amr {
		if !token(method, 64) || seen[method] {
			return errors.New("OIDC methods refused")
		}
		seen[method] = true
	}
	for _, required := range p.RequiredAMR {
		if !seen[required] {
			return errors.New("OIDC methods insufficient")
		}
	}
	authTime, e1 := positiveInteger(claims["auth_time"])
	iat, e2 := positiveDate(claims["iat"])
	exp, e3 := positiveDate(claims["exp"])
	if e1 != nil || e2 != nil || e3 != nil {
		return errors.New("OIDC authentication time refused")
	}
	authDate := new(big.Rat).SetInt64(authTime)
	if authDate.Cmp(iat) > 0 || iat.Cmp(new(big.Rat).SetInt64(now)) > 0 || authDate.Cmp(exp) >= 0 || iat.Cmp(exp) >= 0 {
		return errors.New("OIDC authentication time refused")
	}
	var azp string
	if value, present := claims["azp"]; present {
		if json.Unmarshal(value, &azp) != nil || azp != clientID {
			return errors.New("OIDC authorized party refused")
		}
	}
	if len(audience) > 1 && azp != clientID {
		return errors.New("OIDC authorized party required")
	}
	return nil
}
