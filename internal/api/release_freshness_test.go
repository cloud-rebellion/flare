package api

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestNormalizeReleaseFreshnessRequiresExplicitRelease(t *testing.T) {
	if _, _, err := normalizeReleaseFreshness("", 30); err == nil {
		t.Fatal("empty release selector accepted")
	}
	if _, _, err := normalizeReleaseFreshness("v1", 0); err == nil {
		t.Fatal("zero freshness window accepted")
	}
	if got, window, err := normalizeReleaseFreshness(" v1 ", 30); err != nil || got != "v1" || window != 30 {
		t.Fatalf("normalized release freshness = %q/%d/%v", got, window, err)
	}
}

func TestReleaseFreshnessReasonUsesExplicitDeployAndHeartbeat(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	row := &generated.GetReleaseFreshnessStatsRow{
		Version:        "v1",
		DeployMarkedAt: pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true},
		AgeSeconds:     2 * 60 * 60,
	}
	if got := releaseFreshnessReason(row, "v1", 30); !strings.Contains(got, "no service heartbeat") {
		t.Fatalf("missing-heartbeat reason = %q", got)
	}
	row.LastHeartbeatAt = pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true}
	if got := releaseFreshnessReason(row, "v1", 30); !strings.Contains(got, "heartbeat is stale") {
		t.Fatalf("stale-heartbeat reason = %q", got)
	}
	row.AgeSeconds = 60
	if got := releaseFreshnessReason(row, "v1", 30); got != "" {
		t.Fatalf("fresh heartbeat fired: %q", got)
	}
	if got := releaseFreshnessReason(row, "other", 30); got != "" {
		t.Fatalf("wrong release fired: %q", got)
	}
	row.DeployMarkedAt.Valid = false
	row.AgeSeconds = 7200
	if got := releaseFreshnessReason(row, "v1", 30); got != "" {
		t.Fatalf("unmarked release fired: %q", got)
	}
}

func TestReleaseHeartbeatShapeIsExplicit(t *testing.T) {
	for _, tc := range []struct {
		level, message string
		want           bool
	}{
		{"info", "service-up:web", true},
		{"INFO", "service-up:web", false},
		{"info", "service-up:", false},
		{"info", "service-up:   ", false},
		{"error", "service-up:web", false},
		{"info", "heartbeat:web", false},
	} {
		if got := isReleaseHeartbeat(tc.level, tc.message); got != tc.want {
			t.Errorf("isReleaseHeartbeat(%q,%q) = %v, want %v", tc.level, tc.message, got, tc.want)
		}
	}
}

func TestReleaseHeartbeatUpdateRequiresOneScopedReleaseRow(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/releases.sql")
	if err != nil {
		t.Fatalf("read releases.sql: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: TouchReleaseHeartbeat :execrows")
	if start < 0 {
		t.Fatal("heartbeat update must expose affected-row count")
	}
	section := body[start:]
	if next := strings.Index(section[len("-- name: TouchReleaseHeartbeat :execrows"):], "-- name:"); next >= 0 {
		section = section[:len("-- name: TouchReleaseHeartbeat :execrows")+next]
	}
	for _, want := range []string{"project_id = $1", "org_id = $2", "version = $3"} {
		if !strings.Contains(section, want) {
			t.Fatalf("heartbeat update missing scoped predicate %q", want)
		}
	}
}

func TestDeployMarkerIsSeparateFromEventUpsert(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/releases.sql")
	if err != nil {
		t.Fatalf("read releases.sql: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: MarkReleaseDeploy :one")
	if start < 0 {
		t.Fatal("deploy marker query missing")
	}
	section := body[start:]
	if next := strings.Index(section[len("-- name: MarkReleaseDeploy :one"):], "-- name:"); next >= 0 {
		section = section[:len("-- name: MarkReleaseDeploy :one")+next]
	}
	for _, want := range []string{"deploy_marked_at", "VALUES ($1, $2, $3, $4, now())", "ON CONFLICT (project_id, version) DO UPDATE", "SET deploy_marked_at = now()"} {
		if !strings.Contains(section, want) {
			t.Fatalf("deploy marker query missing %q", want)
		}
	}
	upsertStart := strings.Index(body, "-- name: UpsertRelease :exec")
	if upsertStart < 0 {
		t.Fatal("event upsert query missing")
	}
	upsert := body[upsertStart:]
	if next := strings.Index(upsert[len("-- name: UpsertRelease :exec"):], "-- name:"); next >= 0 {
		upsert = upsert[:len("-- name: UpsertRelease :exec")+next]
	}
	if strings.Contains(upsert, "deploy_marked_at") {
		t.Fatal("event upsert may not mark a deploy")
	}
}

func TestReleaseFreshnessQueryIsTenantScopedAndMarkerGated(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog.sql: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "-- name: GetReleaseFreshnessStats")
	if start < 0 {
		t.Fatal("release freshness query missing")
	}
	section := body[start:]
	if next := strings.Index(section[len("-- name: GetReleaseFreshnessStats"):], "-- name:"); next >= 0 {
		section = section[:len("-- name: GetReleaseFreshnessStats")+next]
	}
	for _, want := range []string{
		"r.project_id = sqlc.arg(project_id)::text",
		"r.org_id = sqlc.arg(org_id)::text",
		"r.version = sqlc.arg(release_version)::text",
		"r.deploy_marked_at IS NOT NULL",
		"last_heartbeat_at",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("freshness query missing %q", want)
		}
	}
	for _, forbidden := range []string{"payload", "stacktrace", "message"} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("freshness query exposed raw field %q", forbidden)
		}
	}
}

func TestReleaseFreshnessRouteIsViewerRead(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	body := string(src)
	readStart := strings.Index(body, "r.Use(s.requireAuth)")
	memberStart := strings.Index(body, `s.requireRole("member")`)
	if readStart < 0 || memberStart < 0 || readStart >= memberStart {
		t.Fatal("router auth/read groups changed")
	}
	readBlock := body[readStart:memberStart]
	if !strings.Contains(readBlock, `r.Get("/projects/{id}/release-freshness", s.handleListReleaseFreshness)`) {
		t.Fatal("release freshness route is not in authenticated viewer read group")
	}
}
