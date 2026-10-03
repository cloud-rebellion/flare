package api

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestNormalizeReleaseErrorRate(t *testing.T) {
	threshold := 0.25
	version, comparison, value, err := normalizeReleaseErrorRate(" web-2026.10.02 ", " GT ", &threshold, 15)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if version != "web-2026.10.02" || comparison != "gt" || value != threshold {
		t.Fatalf("normalized release rule = %q/%q %.2f", version, comparison, value)
	}
	version, comparison, _, err = normalizeReleaseErrorRate("", "", &threshold, 1)
	if err != nil || version != "" || comparison != "gte" {
		t.Fatalf("latest-release defaults = %q/%q err=%v", version, comparison, err)
	}
	for _, tc := range []struct {
		name      string
		version   string
		threshold float64
		window    int32
	}{
		{"version too long", strings.Repeat("x", 129), .2, 15},
		{"invalid comparison", "web", .2, 15},
		{"window below bound", "web", .2, 0},
		{"window above bound", "web", .2, 721},
		{"negative threshold", "web", -.1, 15},
		{"over one threshold", "web", 1.1, 15},
		{"nan threshold", "web", math.NaN(), 15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comparison := "gte"
			if tc.name == "invalid comparison" {
				comparison = "eq"
			}
			if _, _, _, err := normalizeReleaseErrorRate(tc.version, comparison, &tc.threshold, tc.window); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestReleaseErrorRateReason(t *testing.T) {
	stats := &generated.GetReleaseErrorRateStatsRow{ReleaseVersion: "web-2026.10.02", EventCount: 10, ErrorCount: 3}
	if got := releaseErrorRateReason(stats, "", "gte", .3, 15); got == "" || !strings.Contains(got, "web-2026.10.02") {
		t.Fatalf("expected latest release threshold to fire: %q", got)
	}
	if got := releaseErrorRateReason(stats, "other", "gte", .3, 15); got != "" {
		t.Fatalf("mismatched explicit release fired: %q", got)
	}
	if got := releaseErrorRateReason(stats, "web-2026.10.02", "gt", .3, 15); got != "" {
		t.Fatalf("strict threshold fired at equality: %q", got)
	}
	if got := releaseErrorRateReason(&generated.GetReleaseErrorRateStatsRow{}, "", "gte", .1, 15); got != "" {
		t.Fatalf("empty release series fired: %q", got)
	}
}

func TestReleaseErrorRateQueryScopesRecentRelease(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog.sql: %v", err)
	}
	sql := string(raw)
	start := strings.Index(sql, "-- name: GetReleaseErrorRateStats")
	if start < 0 {
		t.Fatal("release error-rate query missing")
	}
	body := sql[start:]
	if end := strings.Index(body, "-- Trace percentile rules"); end >= 0 {
		body = body[:end]
	}
	for _, required := range []string{
		"r.project_id = sqlc.arg(project_id)",
		"r.org_id = sqlc.arg(org_id)",
		"sqlc.arg(release_version)",
		"created_at >= sqlc.arg(since)",
		"e.project_id = sqlc.arg(project_id)",
		"e.org_id = sqlc.arg(org_id)",
		"e.level NOT IN ('info', 'debug')",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("release query missing %q", required)
		}
	}
	for _, forbidden := range []string{"payload", "stacktrace", "attributes", "message"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("release query selected raw event field %q", forbidden)
		}
	}
}
