package alerts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/metrics"
)

// TestDispatchOneRecordsOutcome covers the delivery-status plumbing added for
// the "send test" action and per-channel health: DispatchOne returns the real
// outcome and records it, error paths surface an error, and the recorder is
// skipped for an empty channel id. No network: the log/unsupported/misconfig
// paths never dial, so they dodge the SSRF-guarded client (which blocks the
// loopback an httptest server would use).
func TestDispatchOneRecordsOutcome(t *testing.T) {
	d := NewDispatcher(nil)
	var gotOrg, gotID string
	var gotErr error
	called := 0
	d.Recorder = func(_ context.Context, org, id string, err error) {
		called++
		gotOrg = org
		gotID = id
		gotErr = err
	}

	// log channel always succeeds and records a nil error.
	if err := d.DispatchOne(context.Background(), Channel{ID: "c1", OrgID: "o1", Type: "log"}, Notification{}); err != nil {
		t.Fatalf("log dispatch: unexpected error %v", err)
	}
	if called != 1 || gotID != "c1" || gotOrg != "o1" || gotErr != nil {
		t.Fatalf("log: called=%d org=%q id=%q err=%v", called, gotOrg, gotID, gotErr)
	}

	// unsupported channel type returns and records an error.
	if err := d.DispatchOne(context.Background(), Channel{ID: "c2", OrgID: "o1", Type: "carrier-pigeon"}, Notification{}); err == nil {
		t.Fatal("unsupported type: expected error")
	}
	if called != 2 || gotID != "c2" || gotErr == nil {
		t.Fatalf("unsupported: called=%d id=%q err=%v", called, gotID, gotErr)
	}

	// misconfigured webhook (no url) errors before any dial.
	if err := d.DispatchOne(context.Background(), Channel{ID: "c3", OrgID: "o1", Type: "webhook", Config: json.RawMessage(`{}`)}, Notification{}); err == nil {
		t.Fatal("webhook without url: expected error")
	}
	if called != 3 || gotErr == nil {
		t.Fatalf("webhook misconfig: called=%d err=%v", called, gotErr)
	}

	// an empty channel id must NOT invoke the recorder (ad-hoc dispatch).
	if err := d.DispatchOne(context.Background(), Channel{ID: "", OrgID: "o1", Type: "log"}, Notification{}); err != nil {
		t.Fatalf("empty-id log dispatch: unexpected error %v", err)
	}
	if called != 3 {
		t.Fatalf("empty id should skip recorder, called=%d", called)
	}
}

func TestDispatchRecordsBoundedIntegrationMetrics(t *testing.T) {
	m := metrics.New()
	previous := metrics.Default
	metrics.Default = m
	t.Cleanup(func() { metrics.Default = previous })

	d := NewDispatcher(nil)
	if err := d.DispatchOne(context.Background(), Channel{
		ID: "channel-secret", OrgID: "org-secret", Type: "log",
	}, Notification{}); err != nil {
		t.Fatalf("log dispatch: %v", err)
	}
	if err := d.DispatchOne(context.Background(), Channel{
		ID: "channel-secret-2", OrgID: "org-secret", Type: "untrusted-channel-type",
	}, Notification{}); err == nil {
		t.Fatal("unsupported channel should fail")
	}

	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	for _, want := range []string{
		`flare_integration_requests_total{channel="log",operation="dispatch",outcome="ok",provider="alerts"} 1`,
		`flare_integration_requests_total{channel="other",operation="dispatch",outcome="error",provider="alerts"} 1`,
		`flare_integration_request_duration_seconds_count{channel="log",operation="dispatch",outcome="ok",provider="alerts"} 1`,
		`flare_integration_request_duration_seconds_count{channel="other",operation="dispatch",outcome="error",provider="alerts"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"channel-secret", "org-secret", "untrusted-channel-type"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("untrusted alert value became a metric label: %q\n%s", forbidden, body)
		}
	}
}
