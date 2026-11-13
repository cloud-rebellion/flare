package config

import (
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/retention"
)

// prodEnv sets the minimum env for a production Load, letting a test omit one
// key to assert it is required.
func prodEnv(t *testing.T, omit string) {
	t.Helper()
	vals := map[string]string{
		"ENVIRONMENT":      "production",
		"DATABASE_URL":     "postgres://localhost/flare",
		"SESSION_KEY":      "0123456789abcdef0123456789abcdef0123456789abcdef",
		"CSRF_KEY":         "0123456789abcdef0123456789abcdef",
		"FLARE_SECRET_KEY": "0123456789abcdef0123456789abcdef",
	}
	for k, v := range vals {
		if k == omit {
			continue
		}
		t.Setenv(k, v)
	}
	if omit != "" {
		t.Setenv(omit, "")
	}
}

// TestProductionRequiresFlareSecretKey locks the fail-closed guard: without
// FLARE_SECRET_KEY a production instance must refuse to start, because
// integration secrets (BYOAI key, OIDC secret, GitHub token) would otherwise be
// stored plaintext.
func TestProductionRequiresFlareSecretKey(t *testing.T) {
	prodEnv(t, "FLARE_SECRET_KEY")
	_, err := Load()
	if err == nil {
		t.Fatal("production Load without FLARE_SECRET_KEY must error")
	}
	if !strings.Contains(err.Error(), "FLARE_SECRET_KEY") {
		t.Errorf("error should name FLARE_SECRET_KEY, got: %v", err)
	}
}

// TestProductionRequiresSessionAndCSRF confirms the other required secrets still
// fail closed (guards against a refactor dropping one).
func TestProductionRequiresSessionAndCSRF(t *testing.T) {
	for _, key := range []string{"SESSION_KEY", "CSRF_KEY"} {
		prodEnv(t, key)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("production Load without %s must error naming it, got: %v", key, err)
		}
	}
}

// TestProductionLoadsWithAllSecrets confirms a fully-configured production env
// loads cleanly (so the guard is not over-broad).
func TestProductionLoadsWithAllSecrets(t *testing.T) {
	prodEnv(t, "")
	if _, err := Load(); err != nil {
		t.Fatalf("production Load with all secrets set should succeed, got: %v", err)
	}
}

func TestHubSpotOAuthRequiresPairedCredentialsAndPublicHTTPS(t *testing.T) {
	prodEnv(t, "")
	t.Setenv("BASE_URL", "https://flare.example")
	t.Setenv("FLARE_HUBSPOT_OAUTH_CLIENT_ID", "app-client")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "FLARE_HUBSPOT_OAUTH_CLIENT_SECRET") {
		t.Fatalf("unpaired OAuth app credential accepted: %v", err)
	}
	t.Setenv("FLARE_HUBSPOT_OAUTH_CLIENT_SECRET", "server-secret")
	t.Setenv("BASE_URL", "http://flare.example")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "HTTPS origin") {
		t.Fatalf("insecure OAuth callback origin accepted: %v", err)
	}
	t.Setenv("BASE_URL", "https://flare.example")
	cfg, err := Load()
	if err != nil || cfg.HubSpotOAuthClientID != "app-client" || cfg.HubSpotOAuthClientSecret != "server-secret" {
		t.Fatalf("valid OAuth app configuration rejected: %v", err)
	}
}

// TestSignupIsClosedByDefault locks the default. POST /api/auth/register is the
// self-host first-run path and users.sql has carried the CountUsers bootstrap
// check since it was written; the route shipped ungated anyway, so anyone could
// mint an org on any deployment. The gate is only as good as this default: if
// FLARE_ALLOW_SIGNUP ever reads as true when unset, the gate is a no-op again.
func TestSignupIsClosedByDefault(t *testing.T) {
	prodEnv(t, "")
	t.Setenv("FLARE_ALLOW_SIGNUP", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AllowSignup {
		t.Fatal("AllowSignup must default to false: open registration is not a safe default for a deployed instance")
	}
}

// TestSignupOptInSpellings keeps the escape hatch usable. An operator who wants
// public signup must be able to turn it on the same way every other boolean in
// this file is turned on, or they will set it, see no effect, and conclude the
// flag is broken.
func TestSignupOptInSpellings(t *testing.T) {
	for _, v := range []string{"true", "1", "yes", "on", "TRUE", " True "} {
		t.Run(v, func(t *testing.T) {
			prodEnv(t, "")
			t.Setenv("FLARE_ALLOW_SIGNUP", v)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !cfg.AllowSignup {
				t.Errorf("FLARE_ALLOW_SIGNUP=%q did not enable signup", v)
			}
		})
	}
	for _, v := range []string{"", "false", "0", "no", "off", "maybe"} {
		t.Run("not/"+v, func(t *testing.T) {
			prodEnv(t, "")
			t.Setenv("FLARE_ALLOW_SIGNUP", v)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.AllowSignup {
				t.Errorf("FLARE_ALLOW_SIGNUP=%q must NOT enable signup", v)
			}
		})
	}
}

func TestConnectorSyncSchedulerIsOptInAndBoundedCadenceLoads(t *testing.T) {
	prodEnv(t, "")
	t.Setenv("FLARE_CONNECTOR_SYNC_ENABLED", "")
	t.Setenv("FLARE_CONNECTOR_SYNC_INTERVAL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ConnectorSyncEnabled {
		t.Fatal("connector sync scheduler must be disabled by default")
	}
	if cfg.ConnectorSyncInterval != 5*time.Minute {
		t.Fatalf("default connector sync interval = %v", cfg.ConnectorSyncInterval)
	}

	t.Setenv("FLARE_CONNECTOR_SYNC_ENABLED", "yes")
	t.Setenv("FLARE_CONNECTOR_SYNC_INTERVAL", "45s")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load enabled: %v", err)
	}
	if !cfg.ConnectorSyncEnabled || cfg.ConnectorSyncInterval != 45*time.Second {
		t.Fatalf("enabled connector sync config = %#v", cfg)
	}
}

// COLD_RETENTION_DAYS is the env default for the Parquet archive. Unset keeps
// the archive forever (the behaviour before the setting existed); garbage and
// "off" refuse to boot rather than silently picking a mode.
func TestColdRetentionEnvDefault(t *testing.T) {
	prodEnv(t, "")
	t.Setenv("RETENTION_DAYS", "30")
	t.Setenv("COLD_RETENTION_DAYS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ColdRetentionMode != retention.ColdForever || cfg.ColdRetentionDays != 0 || cfg.RetentionDays != 30 {
		t.Fatalf("unset cold retention = %q/%d, want forever", cfg.ColdRetentionMode, cfg.ColdRetentionDays)
	}

	t.Setenv("COLD_RETENTION_DAYS", "365")
	if cfg, err = Load(); err != nil || cfg.ColdRetentionMode != retention.ColdDays || cfg.ColdRetentionDays != 365 {
		t.Fatalf("COLD_RETENTION_DAYS=365 -> %q/%d %v", cfg.ColdRetentionMode, cfg.ColdRetentionDays, err)
	}

	for _, bad := range []string{"off", "0", "ten", "29"} {
		t.Setenv("COLD_RETENTION_DAYS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("COLD_RETENTION_DAYS=%q with RETENTION_DAYS=30 booted", bad)
		}
	}
}
