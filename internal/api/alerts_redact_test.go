package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/secretbox"
)

func TestRedactChannelConfig(t *testing.T) {
	s := &Server{secrets: secretbox.New("")} // disabled cipher: Decrypt passes plaintext through
	// Slack + webhook URLs are bearer secrets: the secret path must not survive.
	cases := []struct {
		typ    string
		cfg    string
		secret string // must NOT appear in the redacted output
		keep   string // must still appear (host, for identification)
	}{
		{"slack", `{"webhook_url":"https://hooks.slack.com/services/T00/B00/XXXXsecretZZZZ"}`, "XXXXsecretZZZZ", "hooks.slack.com"},
		{"webhook", `{"url":"https://ops.example.com/hook/abc123secretTOKEN"}`, "abc123secretTOKEN", "ops.example.com"},
	}
	for _, c := range cases {
		out := string(s.redactChannelConfig(c.typ, json.RawMessage(c.cfg)))
		if strings.Contains(out, c.secret) {
			t.Errorf("%s: secret survived redaction: %q", c.typ, out)
		}
		if !strings.Contains(out, c.keep) {
			t.Errorf("%s: host dropped, cannot identify channel: %q", c.typ, out)
		}
	}

	// Email destination and log config are not secrets and are preserved.
	if out := string(s.redactChannelConfig("email", json.RawMessage(`{"to":"alerts@corp.com"}`))); !strings.Contains(out, "alerts@corp.com") {
		t.Errorf("email destination should be preserved: %q", out)
	}
}

// With a real key, the webhook secret is encrypted at rest, absent from the
// stored JSON, recovered exactly at the dispatch boundary, and masked for display.
func TestChannelSecretEncryptionRoundTrip(t *testing.T) {
	s := &Server{secrets: secretbox.New("a-real-key")}
	const url = "https://hooks.slack.com/services/T00/B00/SUPERSECRETPATH"
	stored := string(s.encryptChannelConfig("slack", json.RawMessage(`{"webhook_url":"`+url+`"}`)))
	if strings.Contains(stored, "SUPERSECRETPATH") {
		t.Fatalf("secret stored in cleartext: %q", stored)
	}
	if got := string(s.decryptChannelConfig("slack", json.RawMessage(stored))); !strings.Contains(got, url) {
		t.Fatalf("dispatch decrypt did not recover the URL: %q", got)
	}
	if red := string(s.redactChannelConfig("slack", json.RawMessage(stored))); strings.Contains(red, "SUPERSECRETPATH") || !strings.Contains(red, "hooks.slack.com") {
		t.Fatalf("redaction of encrypted config wrong: %q", red)
	}
}

func TestAutomationWebhookSecretsAreEncryptedAndRedacted(t *testing.T) {
	s := &Server{secrets: secretbox.New("a-real-key")}
	const url = "https://worker.example.test/hooks/automation"
	const secret = "automation-signing-secret-1234"
	stored := s.encryptChannelConfig("automation_webhook", json.RawMessage(`{"url":"`+url+`","signing_secret":"`+secret+`"}`))
	if strings.Contains(string(stored), url) || strings.Contains(string(stored), secret) {
		t.Fatalf("automation webhook credentials stored in cleartext: %q", stored)
	}
	opened := string(s.decryptChannelConfig("automation_webhook", stored))
	if !strings.Contains(opened, url) || !strings.Contains(opened, secret) {
		t.Fatalf("automation webhook credentials did not decrypt: %q", opened)
	}
	redacted := string(s.redactChannelConfig("automation_webhook", stored))
	if strings.Contains(redacted, secret) || strings.Contains(redacted, url) || !strings.Contains(redacted, "worker.example.test") {
		t.Fatalf("automation webhook redaction unsafe: %q", redacted)
	}
}
