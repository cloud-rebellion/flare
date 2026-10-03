package flarereport

import "testing"

func TestFlareEnvironmentUsesConfigDimension(t *testing.T) {
	t.Setenv("ENVIRONMENT", "production")
	if got := flareEnvironment(); got != "production" {
		t.Fatalf("environment = %q, want production", got)
	}

	t.Setenv("ENVIRONMENT", " ")
	if got := flareEnvironment(); got != "development" {
		t.Fatalf("empty environment = %q, want development", got)
	}
}
