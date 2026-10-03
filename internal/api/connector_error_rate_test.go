package api

import (
	"math"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/db/generated"
)

func TestNormalizeConnectorErrorRate(t *testing.T) {
	threshold := 0.25
	provider, operation, channel, errorCode, comparison, value, err := normalizeConnectorErrorRate(
		" GitHub ", " CreateIssue ", " HTTP ", " AUTH_EXPIRED ", " GTE ", &threshold, 15,
	)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if provider != "github" || operation != "createissue" || channel != "http" || errorCode != "auth_expired" || comparison != "gte" || value != threshold {
		t.Fatalf("normalized connector rule = %q/%q/%q/%q %q %.2f", provider, operation, channel, errorCode, comparison, value)
	}

	for _, tc := range []struct {
		name      string
		provider  string
		threshold float64
		window    int32
	}{
		{"missing provider", "", 0.2, 15},
		{"negative threshold", "github", -0.1, 15},
		{"over one threshold", "github", 1.1, 15},
		{"too wide window", "github", 0.2, 721},
		{"nan threshold", "github", math.NaN(), 15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, _, _, err := normalizeConnectorErrorRate(tc.provider, "", "", "", "", &tc.threshold, tc.window)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	if _, _, _, _, _, _, err := normalizeConnectorErrorRate("github", "", "", "auth expired", "gte", &threshold, 15); err == nil {
		t.Fatal("expected unsafe connector error code to be rejected")
	}
}

func TestConnectorErrorRateReason(t *testing.T) {
	stats := &generated.GetConnectorErrorRateStatsRow{Attempts: 10, ErrorCount: 3}
	if got := connectorErrorRateReason(stats, "github", "createissue", "http", "", "gte", 0.3, 15); got == "" {
		t.Fatal("expected threshold to fire")
	}
	if got := connectorErrorRateReason(stats, "github", "createissue", "http", "", "gt", 0.3, 15); got != "" {
		t.Fatal("strict threshold should not fire at equality")
	}
	if got := connectorErrorRateReason(stats, "github", "", "", "", "gte", 0.4, 15); got != "" {
		t.Fatal("below threshold should not fire")
	}
	if got := connectorErrorRateReason(&generated.GetConnectorErrorRateStatsRow{}, "github", "", "", "", "lte", 0, 15); got != "" {
		t.Fatal("empty connector series must not fire")
	}
	if got := connectorErrorRateReason(stats, "github", "", "", "", "gte", 0.2, 15); got == "" || !strings.Contains(got, "github/*/*/*") || !strings.Contains(got, "error rate") {
		t.Fatalf("wildcard reason = %q", got)
	}
	if got := connectorErrorRateReason(stats, "github", "createissue", "http", "auth_expired", "gte", 0.2, 15); got == "" || !strings.Contains(got, "github/createissue/http/auth_expired") {
		t.Fatalf("error-code reason = %q", got)
	}
	if got := connectorErrorRateReason(stats, "github", "createissue", "http", "", "gte", 0.2, 15, "conn-prod"); got == "" || !strings.Contains(got, "/connection:conn-prod") {
		t.Fatalf("connection-scoped reason = %q", got)
	}
}
