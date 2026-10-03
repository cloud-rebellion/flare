package api

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestAnalyticsFloatQuery(t *testing.T) {
	tests := []struct {
		name  string
		query string
		key   string
		want  float64
	}{
		{name: "missing uses fallback", query: "", key: "target", want: 0.999},
		{name: "valid value", query: "?target=0.99", key: "target", want: 0.99},
		{name: "malformed uses fallback", query: "?target=wat", key: "target", want: 0.999},
		{name: "other key uses fallback", query: "?target=0.99", key: "latency_ms", want: 1000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/projects/p/analytics/service-level"+tc.query, nil)
			if got := analyticsFloatQuery(r, tc.key, map[string]float64{"target": 0.999, "latency_ms": 1000}[tc.key]); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestServiceLevelRouteIsAuthenticatedViewerRead(t *testing.T) {
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
	if !strings.Contains(readBlock, `r.Get("/projects/{id}/analytics/service-level", s.handleServiceLevel)`) {
		t.Fatal("service-level route is not in the authenticated viewer read group")
	}
}
