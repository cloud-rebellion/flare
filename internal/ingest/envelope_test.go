package ingest

import "testing"

// Exact 3-line envelope @sentry/node v8 sends to /api/{id}/envelope/.
const sentrySDKEnvelope = `{"event_id":"ce17406a98f04de684ef6fd1f6130abd","sent_at":"2026-06-29T20:44:31.984Z","sdk":{"name":"sentry.javascript.node","version":"8.55.2"},"trace":{"environment":"production","public_key":"testkey123","trace_id":"da40c58c887c4dd586909b6cb64196b1"}}
{"type":"event"}
{"exception":{"values":[{"type":"Error","value":"E2E real-sdk: payment gateway timeout"}]},"event_id":"ce17406a98f04de684ef6fd1f6130abd","level":"error","platform":"node"}`

func TestParseEnvelopeSentrySDK(t *testing.T) {
	items, err := ParseEnvelope([]byte(sentrySDKEnvelope))
	if err != nil {
		t.Fatalf("ParseEnvelope error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Type != "event" {
		t.Fatalf("expected item type event, got %q", items[0].Type)
	}
	ev, err := ParseEvent(items[0].Payload)
	if err != nil {
		t.Fatalf("ParseEvent error: %v", err)
	}
	if ev.ExceptionType != "Error" {
		t.Fatalf("expected exception type Error, got %q (title %q)", ev.ExceptionType, ev.Title)
	}
}

// A transaction envelope (sentry-go tracing) must surface as a transaction item,
// not be silently dropped or mis-parsed as an error event.
const sentryTxnEnvelope = `{"event_id":"aa11","sent_at":"2026-07-11T20:00:00Z","trace":{"trace_id":"da40c58c887c4dd586909b6cb64196b1","public_key":"testkey123"}}
{"type":"transaction"}
{"event_id":"aa11","type":"transaction","transaction":"GET /api/users","start_timestamp":1689000000.100,"timestamp":1689000000.600,"contexts":{"trace":{"trace_id":"da40c58c887c4dd586909b6cb64196b1","span_id":"1111111111111111","op":"http.server","status":"ok"}},"spans":[{"span_id":"2222222222222222","parent_span_id":"1111111111111111","trace_id":"da40c58c887c4dd586909b6cb64196b1","op":"db.query","description":"SELECT 1","start_timestamp":1689000000.200,"timestamp":1689000000.300,"status":"ok"}]}`

func TestParseEnvelopeDispatchesTransaction(t *testing.T) {
	items, err := ParseEnvelope([]byte(sentryTxnEnvelope))
	if err != nil {
		t.Fatalf("ParseEnvelope error: %v", err)
	}
	if len(items) != 1 || items[0].Type != "transaction" {
		t.Fatalf("expected 1 transaction item, got %+v", items)
	}
	spans, err := ParseSentryTransaction(items[0].Payload)
	if err != nil {
		t.Fatalf("ParseSentryTransaction error: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("expected root + 1 child span, got %d", len(spans))
	}
	root := spans[0]
	if root.Name != "GET /api/users" || root.SpanID != "1111111111111111" || root.ParentSpanID != "" {
		t.Fatalf("bad root span: %+v", root)
	}
	if root.Kind != "server" || root.Status != "ok" {
		t.Fatalf("root kind/status: kind=%q status=%q", root.Kind, root.Status)
	}
	if root.DurationMs != 500 {
		t.Fatalf("root duration expected 500ms, got %v", root.DurationMs)
	}
	child := spans[1]
	if child.SpanID != "2222222222222222" || child.ParentSpanID != "1111111111111111" {
		t.Fatalf("bad child parentage: %+v", child)
	}
	if child.Name != "SELECT 1" || child.Kind != "client" || child.DurationMs != 100 {
		t.Fatalf("bad child: name=%q kind=%q dur=%v", child.Name, child.Kind, child.DurationMs)
	}
	if child.TraceID != root.TraceID {
		t.Fatalf("child trace_id %q != root %q", child.TraceID, root.TraceID)
	}
}

func TestParseEnvelopeRejectsMalformedTailInsteadOfReturningPrefix(t *testing.T) {
	cases := []string{
		`not-json` + "\n" + `{"type":"event"}` + "\n{}\n",
		"{}\n" + `{"type":"event"}` + "\n{}\n" + `not-json` + "\n{}\n",
		"{}\n" + `{"type":"event"}` + "\n{}\n" + `{"type":"event"}` + "\n",
		"{}\n" + `{"type":"event"}` + "\n{}\n" + `{}` + "\n{}\n",
	}
	for _, body := range cases {
		items, err := ParseEnvelope([]byte(body))
		if err == nil || len(items) != 0 {
			t.Fatalf("malformed tail returned %d accepted prefix items, err=%v", len(items), err)
		}
	}
}

// RFC3339 string timestamps (some SDKs) must parse just like float unix seconds.
func TestParseSentryTransactionRFC3339Timestamps(t *testing.T) {
	payload := `{"transaction":"job","start_timestamp":"2026-07-11T20:00:00Z","timestamp":"2026-07-11T20:00:01.5Z","contexts":{"trace":{"trace_id":"t","span_id":"s","op":"function"}}}`
	spans, err := ParseSentryTransaction([]byte(payload))
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(spans) != 1 || spans[0].DurationMs != 1500 {
		t.Fatalf("expected 1 span dur 1500ms, got %+v", spans)
	}
	if spans[0].Kind != "internal" {
		t.Fatalf("op=function should map to internal kind, got %q", spans[0].Kind)
	}
}

// A transaction with no trace context is unusable and must error, not panic.
func TestParseSentryTransactionNoTrace(t *testing.T) {
	if _, err := ParseSentryTransaction([]byte(`{"transaction":"x"}`)); err == nil {
		t.Fatal("expected error for transaction with no trace context")
	}
}
