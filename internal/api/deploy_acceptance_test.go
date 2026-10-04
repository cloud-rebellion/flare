package api

import (
	"os"
	"strings"
	"testing"
)

func TestDeployAcceptanceReasonUsesGraceAndThreshold(t *testing.T) {
	if got := deployAcceptanceReason(2, 2, 15); !strings.Contains(got, "2") || !strings.Contains(got, "15m") {
		t.Fatalf("deployAcceptanceReason = %q", got)
	}
	for _, tc := range []struct {
		count, threshold, grace int64
	}{
		{0, 1, 15},
		{1, 2, 15},
		{2, 1, 0},
		{2, 1, 721},
		{2, 1_000_001, 15},
	} {
		if got := deployAcceptanceReason(tc.count, int32(tc.threshold), int32(tc.grace)); got != "" {
			t.Fatalf("invalid deploy acceptance bounds fired: %q for %#v", got, tc)
		}
	}
}

func TestDeployAcceptanceQueryIsTenantScopedAndPayloadFree(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog.sql: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: GetDeployAcceptanceStats")
	if start < 0 {
		t.Fatal("deploy acceptance query missing")
	}
	section := body[start:]
	if next := strings.Index(section[len("-- name: GetDeployAcceptanceStats"):], "-- name:"); next >= 0 {
		section = section[:len("-- name: GetDeployAcceptanceStats")+next]
	}
	for _, want := range []string{
		"d.project_id = sqlc.arg(project_id)::text",
		"d.org_id = sqlc.arg(org_id)::text",
		"d.stage = 'deploy'",
		"d.status = 'passed'",
		"d.verification_status = 'verified'",
		"d.completed_at >= sqlc.arg(since)",
		"d.completed_at < sqlc.arg(grace_cutoff)",
		"s.stage = 'synthetic'",
		"s.status = 'passed'",
		"s.verification_status = 'verified'",
		"s.target_revision = d.target_revision",
		"s.completed_at >= d.completed_at",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("deploy acceptance query missing %q", want)
		}
	}
	for _, forbidden := range []string{"prompt", "completion", "payload", "select d.run_ref", "select s.evidence_ref"} {
		if strings.Contains(strings.ToLower(section), forbidden) {
			t.Fatalf("deploy acceptance query exposed raw field %q", forbidden)
		}
	}
	watchStart := strings.Index(body, "-- name: ListWatchdogRules")
	if watchStart < 0 || !strings.Contains(body[watchStart:], "'deploy_acceptance'") {
		t.Fatal("watchdog rule list does not include deploy_acceptance")
	}
}
