package api

import (
	"os"
	"strings"
	"testing"
)

func TestNormalizeLogSeverity(t *testing.T) {
	severity, threshold, window, err := normalizeLogSeverity(" WARNING ", 10, 15)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if severity != "warn" || threshold != 10 || window != 15 {
		t.Fatalf("normalized log rule = %q/%d/%d", severity, threshold, window)
	}
	severity, _, _, err = normalizeLogSeverity("critical", 1, 1)
	if err != nil || severity != "fatal" {
		t.Fatalf("critical alias = %q err=%v", severity, err)
	}
	for _, tc := range []struct {
		name      string
		severity  string
		threshold int32
		window    int32
	}{
		{"missing severity", "", 1, 15},
		{"unknown severity", "notice", 1, 15},
		{"threshold below bound", "error", 0, 15},
		{"threshold above bound", "error", 1_000_001, 15},
		{"window below bound", "error", 1, 0},
		{"window above bound", "error", 1, 721},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := normalizeLogSeverity(tc.severity, tc.threshold, tc.window); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestLogSeverityReason(t *testing.T) {
	if got := logSeverityReason(9, "error", 10, 15); got != "" {
		t.Fatalf("below threshold fired: %q", got)
	}
	got := logSeverityReason(10, "error", 10, 15)
	if got == "" || !strings.Contains(got, "error") || !strings.Contains(got, "10 records") {
		t.Fatalf("threshold reason = %q", got)
	}
	if got := logSeverityReason(0, "error", 1, 15); got != "" {
		t.Fatalf("empty series fired: %q", got)
	}
	if got := logSeverityReason(100, "error", 0, 15); got != "" {
		t.Fatalf("invalid threshold fired: %q", got)
	}
}

func TestLogSeverityQueryScopesTenantProjectAndWindow(t *testing.T) {
	raw, err := os.ReadFile("../db/queries/watchdog.sql")
	if err != nil {
		t.Fatalf("read watchdog.sql: %v", err)
	}
	sql := string(raw)
	start := strings.Index(sql, "-- name: GetLogSeverityStats")
	if start < 0 {
		t.Fatal("log severity query missing")
	}
	body := sql[start:]
	if end := strings.Index(body, "-- Release error-rate rules"); end >= 0 {
		body = body[:end]
	}
	for _, required := range []string{
		"project_id = sqlc.arg(project_id)",
		"org_id = sqlc.arg(org_id)",
		"severity = sqlc.arg(severity)",
		"observed_at >= sqlc.arg(since)",
		"severity = 'warning'",
		"severity = 'critical'",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("query missing %q", required)
		}
	}
	for _, forbidden := range []string{"body", "attributes", "trace_id", "span_id"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("query selected raw log field %q", forbidden)
		}
	}
}
