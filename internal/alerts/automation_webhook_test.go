package alerts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type automationWebhookCaptureTransport struct {
	body    []byte
	headers http.Header
}

func (c *automationWebhookCaptureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var err error
	c.body, err = io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	c.headers = r.Header.Clone()
	return &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    r,
	}, nil
}

func TestAutomationWebhookSendsSignedPayloadWithoutTelemetryText(t *testing.T) {
	const secret = "automation-signing-secret-1234"
	capture := new(automationWebhookCaptureTransport)

	d := NewDispatcher(nil)
	// Replace the SSRF-guarded production client only for this transport test;
	// channel creation and the real dispatcher still use the guarded one.
	d.client = &http.Client{Transport: capture}
	n := Notification{
		ProjectID: "project-1", IssueID: "issue-1", Title: "credential in issue title",
		Culprit: "Authorization: Bearer secret", Reason: "New issue", EventCount: 3,
	}
	if err := d.DispatchOne(context.Background(), Channel{
		Type:   "automation_webhook",
		Config: json.RawMessage(`{"url":"https://worker.example.test/flare","signing_secret":"` + secret + `"}`),
	}, n); err != nil {
		t.Fatalf("automation webhook: %v", err)
	}
	if strings.Contains(string(capture.body), "credential in issue title") || strings.Contains(string(capture.body), "Authorization") {
		t.Fatalf("webhook payload leaked issue text: %s", capture.body)
	}
	var payload automationWebhookPayload
	if err := json.Unmarshal(capture.body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Schema != automationWebhookSchema || payload.Event != "new_issue" || payload.ProjectID != "project-1" || payload.IssueID != "issue-1" || payload.EventCount != 3 {
		t.Fatalf("payload = %#v", payload)
	}
	if got := capture.headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	timestamp := capture.headers.Get("X-Flare-Automation-Timestamp")
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	age := time.Since(time.Unix(seconds, 0))
	if err != nil || age < 0 || age > automationWebhookReplayWindow {
		t.Fatalf("timestamp = %q, err=%v", timestamp, err)
	}
	if capture.headers.Get("X-Flare-Automation-Event") != "new_issue" {
		t.Fatalf("event header = %q", capture.headers.Get("X-Flare-Automation-Event"))
	}
	if got := capture.headers.Get("X-Flare-Automation-Signature"); got != automationWebhookSignature(secret, timestamp, capture.body) {
		t.Fatalf("signature = %q, want %q", got, automationWebhookSignature(secret, timestamp, capture.body))
	}
}

func TestAutomationWebhookBodyBoundsAndEventVocabulary(t *testing.T) {
	if got := automationWebhookEvent("Spike: 4 events in 15m"); got != "spike" {
		t.Fatalf("spike event = %q", got)
	}
	if got := automationWebhookEvent("untrusted reason"); got != "alert" {
		t.Fatalf("unknown event = %q", got)
	}
	if _, err := automationWebhookBody(Notification{IssueID: "i", EventCount: 1}, time.Now()); err == nil {
		t.Fatal("missing project id should be rejected")
	}
	if body, err := automationWebhookBody(Notification{ProjectID: "p", Reason: "service unavailable", EventCount: 1}, time.Now()); err != nil {
		t.Fatalf("non-issue watchdog notification should be accepted: %v", err)
	} else if !strings.Contains(string(body), `"issue_id":""`) {
		t.Fatalf("non-issue payload must retain the fixed issue_id field: %s", body)
	}
	if _, err := automationWebhookBody(Notification{ProjectID: "p", IssueID: "i", EventCount: -1}, time.Now()); err == nil {
		t.Fatal("negative event count should be rejected")
	}
	if _, err := automationWebhookBody(Notification{ProjectID: "p", IssueID: "i", EventCount: maxAutomationWebhookEventCount + 1}, time.Now()); err == nil {
		t.Fatal("unbounded event count should be rejected")
	}
}
