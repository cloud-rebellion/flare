package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestArtifactMetadataContainsOnlyBoundedParseStatus(t *testing.T) {
	body, err := json.Marshal(artifactResponse{
		ID: "a", Release: "r", Name: "app.min.js", Size: 12,
		ParseStatus: "invalid_mappings",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, `"parse_status":"invalid_mappings"`) {
		t.Fatalf("parse status missing from artifact metadata: %s", text)
	}
	if strings.Contains(text, `"content"`) {
		t.Fatalf("artifact metadata leaked map content: %s", text)
	}
}

func TestSourceMapPersistenceStoresStatusAndFiltersInvalidMaps(t *testing.T) {
	query, err := os.ReadFile("../db/queries/artifacts.sql")
	if err != nil {
		t.Fatal(err)
	}
	q := strings.ToLower(string(query))
	for _, required := range []string{
		"parse_status", "parse_status in ('unvalidated', 'valid')",
		"set content = excluded.content, parse_status = excluded.parse_status",
	} {
		if !strings.Contains(q, required) {
			t.Fatalf("source-map query missing %q", required)
		}
	}
	for _, forbidden := range []string{"request_body", "response_body", "bearer", "session_id", "user_id"} {
		if strings.Contains(q, forbidden) {
			t.Fatalf("source-map query contains forbidden field %q", forbidden)
		}
	}
	migration, err := os.ReadFile("../db/migrations/045_source_map_validation.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := strings.ToLower(string(migration))
	for _, required := range []string{
		"add column parse_status text not null default 'unvalidated'",
		"check (parse_status in ('unvalidated', 'valid', 'invalid_json', 'invalid_mappings', 'unsupported'))",
	} {
		if !strings.Contains(m, required) {
			t.Fatalf("source-map migration missing %q", required)
		}
	}
}
