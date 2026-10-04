package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A retry signs the same bounded body with a fresh replay timestamp. The
// receiver uses the verified body's SHA as its idempotency key; no unsigned
// header is promoted to an authorization or deduplication identity.
func TestQueuedAutomationWebhookBodyStableAcrossAttempts(t *testing.T) {
	const secret = "automation-signing-secret-1234"
	capture := new(automationWebhookCaptureTransport)
	d := NewDispatcher(nil)
	d.client = &http.Client{Transport: capture}
	n := Notification{
		ProjectID: "project-1", IssueID: "issue-1", EventCount: 3,
		ObservedAt:      time.Date(2026, 10, 4, 12, 30, 0, 123000000, time.UTC),
		AutomationEvent: "new_issue", Title: "untrusted title never forwarded",
	}
	ch := Channel{Type: "automation_webhook", Config: json.RawMessage(`{"url":"https://worker.example.test/flare","signing_secret":"` + secret + `"}`)}
	if err := d.DispatchOne(context.Background(), ch, n); err != nil {
		t.Fatal(err)
	}
	first := bytes.Clone(capture.body)
	firstSignature := capture.headers.Get("X-Flare-Automation-Signature")
	firstTime := capture.headers.Get("X-Flare-Automation-Timestamp")
	if firstSignature != automationWebhookSignature(secret, firstTime, first) {
		t.Fatal("first signature did not cover the stable body")
	}
	if err := d.DispatchOne(context.Background(), ch, n); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, capture.body) {
		t.Fatalf("retry changed signed body: %s -> %s", first, capture.body)
	}
	if capture.headers.Get("X-Flare-Automation-Signature") != automationWebhookSignature(secret, capture.headers.Get("X-Flare-Automation-Timestamp"), capture.body) {
		t.Fatal("retry signature invalid")
	}
	if capture.headers.Get("X-Flare-Automation-Delivery-ID") != "" {
		t.Fatal("unsigned delivery identity leaked into webhook headers")
	}
}
