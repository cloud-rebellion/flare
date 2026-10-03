package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestUsageOperationCanonicalizationRejectsCallerText(t *testing.T) {
	tests := []struct {
		surface string
		value   string
		want    string
	}{
		{"dsn", "../../secret", "other"},
		{"api_key", "Bearer live-secret", "other"},
		{"mcp", "tool_unknown", "other"},
		{"mcp", "tool_query_metrics", "tool_query_metrics"},
	}
	for _, tc := range tests {
		t.Run(tc.surface+"/"+tc.value, func(t *testing.T) {
			if got := canonicalUsageOperation(tc.surface, tc.value); got != tc.want {
				t.Fatalf("canonicalUsageOperation(%q, %q) = %q, want %q", tc.surface, tc.value, got, tc.want)
			}
		})
	}
}

func TestDSNUsageOperationHasFixedVocabulary(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/api/123/envelope", "envelope"},
		{"/api/123/events", "event"},
		{"/api/123/logs", "logs"},
		{"/api/123/connector-attempts", "connector_attempts"},
		{"/api/123/checkins/nightly", "checkins"},
		{"/otlp/v1/metrics", "otlp_metrics"},
		{"/unknown/attacker-controlled", "other"},
	}
	for _, tc := range tests {
		r := httptest.NewRequest(http.MethodPost, tc.path, nil)
		if got := dsnUsageOperation(r); got != tc.want {
			t.Fatalf("dsnUsageOperation(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestMCPUsageOperationAllowListsTools(t *testing.T) {
	if got := mcpUsageOperation(mcpRequest{Method: "tools/call", Params: []byte(`{"name":"query_metrics"}`)}); got != "tool_query_metrics" {
		t.Fatalf("known MCP tool operation = %q", got)
	}
	if got := mcpUsageOperation(mcpRequest{Method: "tools/call", Params: []byte(`{"name":"inject_secret"}`)}); got != "tools_call" {
		t.Fatalf("unknown MCP tool operation = %q, want tools_call", got)
	}
	if got := mcpUsageOperation(mcpRequest{Method: "bogus/" + strings.Repeat("x", 200)}); got != "other" {
		t.Fatalf("unknown MCP method operation = %q, want other", got)
	}
}

func TestUsageResponseOmitsEmptyIdentifiersAndComputesRate(t *testing.T) {
	now := time.Now().UTC()
	row := &generated.ListUsageBucketsByOrgRow{
		Surface: "mcp", Operation: "tool_query_metrics", RequestCount: 4, ErrorCount: 1,
		FirstSeenAt: pgtype.Timestamptz{Time: now, Valid: true},
		LastSeenAt:  pgtype.Timestamptz{Time: now, Valid: true},
	}
	v := usageResponse{
		ProjectID: row.ProjectID, CredentialID: row.CredentialID,
		Surface: row.Surface, Operation: row.Operation,
		Requests: row.RequestCount, Errors: row.ErrorCount, Denied: row.DeniedCount,
		FirstSeenAt: receiptTimestamp(row.FirstSeenAt), LastSeenAt: receiptTimestamp(row.LastSeenAt),
	}
	if v.Requests > 0 {
		v.ErrorRate = float64(v.Errors) / float64(v.Requests)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if strings.Contains(text, "project_id") || strings.Contains(text, "credential_id") {
		t.Fatalf("empty identifiers leaked into response: %s", text)
	}
	if !strings.Contains(text, `"error_rate":0.25`) {
		t.Fatalf("error rate missing from response: %s", text)
	}
}

func TestUsageQueryBounds(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", defaultUsageHours},
		{"?hours=0", defaultUsageHours},
		{"?hours=9999", maxUsageHours},
		{"?hours=24junk", defaultUsageHours},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/usage"+tc.query, nil)
		if got := usageHours(r); got != tc.want {
			t.Fatalf("usageHours(%q) = %d, want %d", tc.query, got, tc.want)
		}
	}
}

func TestUsageStorageAndQueryStayTenantScopedAndPayloadFree(t *testing.T) {
	query, err := os.ReadFile("../db/queries/usage.sql")
	if err != nil {
		t.Fatal(err)
	}
	q := strings.ToLower(string(query))
	for _, want := range []string{"where org_id = $1", "on conflict (org_id, project_id, credential_id", "sum(request_count)"} {
		if !strings.Contains(q, want) {
			t.Fatalf("usage query missing %q", want)
		}
	}
	for _, forbidden := range []string{"key_hash", "bearer", "request_body", "response_body", "session_id", "user_id", "ip_address"} {
		if strings.Contains(q, forbidden) {
			t.Fatalf("usage query contains forbidden secret or identity column %q", forbidden)
		}
	}
	migration, err := os.ReadFile("../db/migrations/042_usage_buckets.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`\s+`).ReplaceAllString(strings.ToLower(string(migration)), " ")
	for _, want := range []string{"org_id text not null references orgs", "surface text not null check (surface in ('dsn', 'api_key', 'mcp'))", "primary key (org_id"} {
		if !strings.Contains(m, want) {
			t.Fatalf("usage migration missing %q", want)
		}
	}
}
