package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bright-interaction/flare/internal/retention"
)

// Every surface that reports retention reads the effective (stored) policy,
// not the env (audit P1-4). With "7 days, no archive" stored, /readyz used to
// report 30 days and export_before_drop, and the settings view had no field
// for FLARE_ALLOW_DROP_WITHOUT_EXPORT at all.
func TestRetentionSurfacesReportTheStoredPolicy(t *testing.T) {
	s, _, _, call := retentionHarness(t)
	s.cfg.AllowDropWithoutExport = true
	if w := call("PUT", "/api/settings/retention", `{"hot_retention_days":7,"cold_mode":"off","confirm_reduction":true}`); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}

	var ready struct {
		Retention healthRetention `json:"retention"`
	}
	if err := json.Unmarshal(call("GET", "/readyz", "").Body.Bytes(), &ready); err != nil {
		t.Fatal(err)
	}
	if ready.Retention.Days != 7 || ready.Retention.Policy != "drop_without_export" || ready.Retention.ExportRequired {
		t.Fatalf("/readyz retention = %+v, want 7 days, drop_without_export, export not required", ready.Retention)
	}

	var view map[string]any
	if err := json.Unmarshal(call("GET", "/api/settings/retention", "").Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if v, ok := view["allow_drop_without_export"]; !ok || v != true {
		t.Fatalf("settings view must surface allow_drop_without_export, got %v", view["allow_drop_without_export"])
	}
	// Archiving on, cold tier down, drop allowed: the view must say the days
	// are deleted, not paused or archived.
	if w := call("PUT", "/api/settings/retention", `{"hot_retention_days":30,"cold_mode":"forever"}`); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	var v retentionResponse
	_ = json.Unmarshal(call("GET", "/api/settings/retention", "").Body.Bytes(), &v)
	if v.HotPausedReason == "" || v.HotPausedReason == "Aged partitions are kept because they cannot be archived while the cold tier is down." {
		t.Fatalf("hot_paused_reason with allow-drop = %q, want the deleted-without-archive notice", v.HotPausedReason)
	}
}

// Log search (REST and MCP) can reach every day the longest hot window keeps.
func TestLogWindowCoversTheLongestHotRetention(t *testing.T) {
	if maxLogWindowHours < retention.MaxHotDays*24 {
		t.Fatalf("maxLogWindowHours = %d, below the %d days an admin may keep hot", maxLogWindowHours, retention.MaxHotDays)
	}
}
