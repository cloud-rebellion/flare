package api

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestConnectorSourceKindSelection(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  string
		ok    bool
	}{
		{"", "server_observed", true},
		{"?source_kind=client_reported", "client_reported", true},
		{"?source_kind=legacy_unknown", "legacy_unknown", true},
		{"?source_kind=provider_attested", "", false},
	} {
		got, ok := connectorSourceKind(httptest.NewRequest("GET", "/api/connector-health"+tc.query, nil))
		if got != tc.want || ok != tc.ok {
			t.Fatalf("source_kind %q = %q/%v, want %q/%v", tc.query, got, ok, tc.want, tc.ok)
		}
	}
	if _, err := normalizeConnectorSourceKind("client_reported"); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeConnectorSourceKind("legacy_unknown"); err == nil {
		t.Fatal("legacy observations must not be eligible for new alert rules")
	}
}

func TestNormalizeConnectorAttemptRequest(t *testing.T) {
	got, err := normalizeConnectorAttemptRequest(connectorAttemptRequest{
		Provider: " hubspot ", Operation: "contacts.upsert", Channel: "http",
		Outcome: "OK", StatusClass: "5xx", ErrorCode: "ignored", ElapsedMS: 42,
	})
	if err != nil {
		t.Fatalf("normalize valid success: %v", err)
	}
	if got.Provider != "hubspot" || got.StatusClass != "2xx" || got.ErrorCode != "" {
		t.Fatalf("normalized success = %#v", got)
	}

	for _, tc := range []struct {
		name string
		in   connectorAttemptRequest
	}{
		{"missing provider", connectorAttemptRequest{Operation: "sync", Channel: "http", Outcome: "ok"}},
		{"unsafe operation", connectorAttemptRequest{Provider: "hubspot", Operation: "sync?x", Channel: "http", Outcome: "ok"}},
		{"unknown outcome", connectorAttemptRequest{Provider: "hubspot", Operation: "sync", Channel: "http", Outcome: "maybe"}},
		{"bad status class", connectorAttemptRequest{Provider: "hubspot", Operation: "sync", Channel: "http", Outcome: "error", StatusClass: "503"}},
		{"error without reason", connectorAttemptRequest{Provider: "hubspot", Operation: "sync", Channel: "http", Outcome: "error"}},
		{"negative elapsed", connectorAttemptRequest{Provider: "hubspot", Operation: "sync", Channel: "http", Outcome: "ok", ElapsedMS: -1}},
		{"oversized token", connectorAttemptRequest{Provider: strings.Repeat("x", 33), Operation: "sync", Channel: "http", Outcome: "ok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := normalizeConnectorAttemptRequest(tc.in); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestConnectorAttemptRouteIsDSNIngest(t *testing.T) {
	content, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	body := string(content)
	if !strings.Contains(body, `r.Post("/{projectID}/connector-attempts", s.handleConnectorAttempt)`) {
		t.Fatal("connector attempt route is missing from the DSN ingest group")
	}
}
