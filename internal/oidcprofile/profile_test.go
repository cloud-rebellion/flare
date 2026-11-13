package oidcprofile

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStrictSignedAssuranceProfile(t *testing.T) {
	p, err := Parse("urn:example:passkey-uv:v1", "pop,mfa")
	if err != nil {
		t.Fatal(err)
	}
	base := `{"acr":"urn:example:passkey-uv:v1","amr":["pop","mfa","extra"],"auth_time":100,"iat":200,"exp":400,"azp":"client"}`
	if err := p.Validate([]byte(base), "client", []string{"client", "second"}, 300); err != nil {
		t.Fatal(err)
	}
	for _, numbers := range [][2]string{{"200.5", "400.5"}, {"2e2", "4e2"}, {"2.005e2", "4.005e2"}} {
		raw := strings.Replace(strings.Replace(base, `"iat":200`, `"iat":`+numbers[0], 1), `"exp":400`, `"exp":`+numbers[1], 1)
		if err := p.Validate([]byte(raw), "client", []string{"client", "second"}, 300); err != nil {
			t.Fatal("valid NumericDate rejected", err)
		}
	}
	for _, tc := range []struct{ name, old, new string }{
		{"weak class", "urn:example:passkey-uv:v1", "phr"},
		{"case sensitive", "urn:example:passkey-uv:v1", "urn:example:passkey-UV:v1"},
		{"weak methods", `["pop","mfa","extra"]`, `["otp"]`},
		{"duplicate methods", `["pop","mfa","extra"]`, `["pop","mfa","mfa"]`},
		{"scalar methods", `["pop","mfa","extra"]`, `"mfa"`},
		{"mixed methods", `["pop","mfa","extra"]`, `["pop","mfa",1]`},
		{"zero time", `"auth_time":100`, `"auth_time":0`},
		{"string time", `"auth_time":100`, `"auth_time":"100"`},
		{"fraction time", `"auth_time":100`, `"auth_time":100.0`},
		{"exponent time", `"auth_time":100`, `"auth_time":1e2`},
		{"future auth", `"auth_time":100`, `"auth_time":301`},
		{"future issued", `"iat":200`, `"iat":301`},
		{"future fraction", `"iat":200`, `"iat":300.00001`},
		{"numeric time string", `"iat":200`, `"iat":"200"`},
		{"unbounded exponent", `"exp":400`, `"exp":1e9999999`},
		{"expiry boundary", `"exp":400`, `"exp":200`},
		{"wrong party", `"azp":"client"`, `"azp":"other"`},
		{"duplicate acr", `"acr":`, `"acr":"old","acr":`},
		{"duplicate auth time", `"auth_time":100`, `"auth_time":1,"auth_time":100`},
		{"duplicate issuer", `"acr":`, `"iss":"a","iss":"b","acr":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if p.Validate([]byte(strings.Replace(base, tc.old, tc.new, 1)), "client", []string{"client", "second"}, 300) == nil {
				t.Fatal("invalid signed profile accepted")
			}
		})
	}
	var claims map[string]any
	_ = json.Unmarshal([]byte(base), &claims)
	for _, key := range []string{"acr", "amr", "auth_time", "iat", "exp", "azp"} {
		t.Run("missing "+key, func(t *testing.T) {
			copy := map[string]any{}
			for k, v := range claims {
				if k != key {
					copy[k] = v
				}
			}
			b, _ := json.Marshal(copy)
			if p.Validate(b, "client", []string{"client", "second"}, 300) == nil {
				t.Fatal("missing claim accepted")
			}
		})
	}
	if (Policy{}).Validate([]byte(`not claims`), "client", nil, 0) != nil {
		t.Fatal("empty policy changed generic behavior")
	}
}

func TestProfileConfigurationIsExplicitAndCanonical(t *testing.T) {
	for _, pair := range [][2]string{{"", "mfa"}, {"class", ""}, {"class", "mfa,mfa"}, {"class name", "mfa"}, {"class", "mfa,,pop"}, {"class", strings.Repeat("x", 65)}} {
		if _, err := Parse(pair[0], pair[1]); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
	off, err := Parse("", "")
	if err != nil || off.Enabled() || off.Digest() != "" {
		t.Fatal("default policy changed")
	}
	a, _ := Parse("class", "pop,mfa")
	b, _ := Parse("class", "mfa,pop")
	if a.Digest() != b.Digest() {
		t.Fatal("method order changed effective policy")
	}
}

func TestSignedJSONUnicodeIsNotRepairedIntoEvidence(t *testing.T) {
	p, _ := Parse("class", "pop,mfa")
	prefix := `{"acr":"class","auth_time":100,"iat":200,"exp":400,"amr":["pop","mfa",`
	for _, method := range []string{`"\ud800"`, `"\udc00"`, `"\ud800\ud800"`, `"\ud800\\udc00"`} {
		if p.Validate([]byte(prefix+method+`]}`), "client", []string{"client"}, 300) == nil {
			t.Fatal("invalid Unicode repaired into signed evidence")
		}
	}
	for _, method := range []string{`"\ud83e\udeaa"`, `"🪪"`, `"�"`, `"\ufffd"`, `"\\ud800"`} {
		if err := p.Validate([]byte(prefix+method+`]}`), "client", []string{"client"}, 300); err != nil {
			t.Fatal("valid Unicode or quoted slash refused", err)
		}
	}
	invalid := append([]byte(prefix+`"`), 0xff)
	invalid = append(invalid, []byte(`"]}`)...)
	if p.Validate(invalid, "client", []string{"client"}, 300) == nil {
		t.Fatal("invalid raw UTF8 accepted")
	}
}
