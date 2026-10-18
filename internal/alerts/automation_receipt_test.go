package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/automationdelivery"
)

type receiptTrip func(*http.Request) (*http.Response, error)

func (f receiptTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAutomationReactorAcknowledgementContract(t *testing.T) {
	const real = `{"accepted":true,"deduped":false,"run_id":"run_123"}`
	for _, tc := range []struct {
		name, body, contentType, event string
		status                         int
		valid                          bool
	}{
		{"new run", real, "application/json", "new_issue", 202, true},
		{"deduped run", strings.Replace(real, `false`, `true`, 1), "application/json; charset=utf-8", "new_issue", 200, true},
		{"signed no-op", `{"accepted":true,"test":true,"dispatched":false}`, "application/json", "test", 200, true},
		{"empty", "", "application/json", "new_issue", 204, false},
		{"HTML success", `<html>private diagnostic</html>`, "text/html", "new_issue", 200, false},
		{"wrong content type", real, "text/plain", "new_issue", 200, false},
		{"missing content type", real, "", "new_issue", 200, false},
		{"not accepted", strings.Replace(real, `"accepted":true`, `"accepted":false`, 1), "application/json", "new_issue", 202, false},
		{"missing run", `{"accepted":true,"deduped":false}`, "application/json", "new_issue", 202, false},
		{"empty run", strings.Replace(real, `run_123`, ``, 1), "application/json", "new_issue", 202, false},
		{"URL as run", strings.Replace(real, `run_123`, `https://evil.invalid/token`, 1), "application/json", "new_issue", 202, false},
		{"unsafe run", strings.Replace(real, `run_123`, `..`, 1), "application/json", "new_issue", 202, false},
		{"oversized run", strings.Replace(real, `run_123`, strings.Repeat("r", 129), 1), "application/json", "new_issue", 202, false},
		{"missing deduped", `{"accepted":true,"run_id":"run_123"}`, "application/json", "new_issue", 202, false},
		{"null field", strings.Replace(real, `false`, `null`, 1), "application/json", "new_issue", 202, false},
		{"duplicate accepted", strings.Replace(real, `"accepted":true`, `"accepted":false,"accepted":true`, 1), "application/json", "new_issue", 202, false},
		{"escaped duplicate", strings.Replace(real, `"accepted":true`, `"accepted":false,"\u0061ccepted":true`, 1), "application/json", "new_issue", 202, false},
		{"extra provider text", strings.Replace(real, `"accepted":true`, `"provider_error":"private diagnostic","accepted":true`, 1), "application/json", "new_issue", 202, false},
		{"trailing JSON", real + `{}`, "application/json", "new_issue", 202, false},
		{"oversized", real + strings.Repeat(" ", automationdelivery.MaxReceiptBytes), "application/json", "new_issue", 202, false},
		{"test asserting run", real, "application/json", "test", 200, false},
		{"test started work", `{"accepted":true,"test":true,"dispatched":true}`, "application/json", "test", 200, false},
		{"server failure", real, "application/json", "new_issue", 500, false},
		{"redirect", real, "application/json", "new_issue", 302, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDispatcher(nil)
			var sent []byte
			calls := 0
			d.client.Transport = receiptTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				sent, _ = io.ReadAll(r.Body)
				h := make(http.Header)
				if tc.contentType != "" {
					h.Set("Content-Type", tc.contentType)
				}
				if tc.status == 302 {
					h.Set("Location", "https://elsewhere.invalid/stolen")
				}
				return &http.Response{StatusCode: tc.status, Header: h, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})
			ack, err := d.DispatchAutomationNotice(context.Background(), Channel{Type: "automation_webhook", Config: json.RawMessage(`{"url":"https://worker.example.test/secret-path","signing_secret":"private-key-never-journaled","receipt_mode":"reactor-v1"}`)}, Notification{ProjectID: "project-1", IssueID: "issue-1", AutomationEvent: tc.event, ObservedAt: time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)})
			if calls != 1 {
				t.Fatalf("response changed destination or repeated send: %d", calls)
			}
			if !tc.valid {
				if err == nil || ack != nil || strings.Contains(err.Error(), "private diagnostic") || strings.Contains(err.Error(), "secret-path") {
					t.Fatalf("unsafe response accepted: ack=%+v err=%v", ack, err)
				}
				return
			}
			if err != nil || ack == nil || !ack.Valid(tc.event, automationdelivery.Digest(sent)) {
				t.Fatalf("valid receipt refused: %+v %v", ack, err)
			}
			if tc.event == "test" && (ack.RunID != "" || ack.Deduped != nil || !ack.Test) {
				t.Fatalf("test asserted enqueue: %+v", ack)
			}
			encoded, _ := json.Marshal(ack)
			if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "https:") {
				t.Fatalf("receipt retained destination/credential: %s", encoded)
			}
		})
	}
}

func TestAutomationHTTPCompatibilityAndModeRefusal(t *testing.T) {
	for _, mode := range []string{"", `,"receipt_mode":"http"`, `,"receipt_mode":"unknown"`, `,"receipt_mode":null`} {
		d := NewDispatcher(nil)
		calls := 0
		d.client.Transport = receiptTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("not a receipt")), Request: r}, nil
		})
		ack, err := d.DispatchAutomationNotice(context.Background(), Channel{Type: "automation_webhook", Config: json.RawMessage(`{"url":"https://worker.example.test/path","signing_secret":"secret-key"` + mode + `}`)}, Notification{ProjectID: "project-1", Reason: "New issue"})
		if strings.Contains(mode, "unknown") || strings.Contains(mode, "null") {
			if err == nil || calls != 0 || ack != nil {
				t.Fatalf("invalid mode sent: calls=%d ack=%+v err=%v", calls, ack, err)
			}
			continue
		}
		if err != nil || ack == nil || ack.Mode != automationdelivery.HTTP || ack.RunID != "" || ack.Deduped != nil || calls != 1 {
			t.Fatalf("legacy HTTP compatibility broke: %+v %v calls=%d", ack, err, calls)
		}
	}
}

func TestAutomationReceiptReadFailureDoesNotEchoTransportData(t *testing.T) {
	d := NewDispatcher(nil)
	d.client.Transport = receiptTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 202, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: failingReceiptBody{}, Request: r}, nil
	})
	_, err := d.DispatchAutomationNotice(context.Background(), Channel{Type: "automation_webhook", Config: json.RawMessage(`{"url":"https://worker.example.test/path","signing_secret":"secret-key","receipt_mode":"reactor-v1"}`)}, Notification{ProjectID: "p", Reason: "New issue"})
	if !errors.Is(err, automationdelivery.ErrReceipt) || strings.Contains(err.Error(), "private") {
		t.Fatalf("read failure leaked diagnostics: %v", err)
	}
}

type failingReceiptBody struct{}

func (failingReceiptBody) Read([]byte) (int, error) {
	return 0, errors.New("private receiver diagnostic")
}
func (failingReceiptBody) Close() error { return nil }
