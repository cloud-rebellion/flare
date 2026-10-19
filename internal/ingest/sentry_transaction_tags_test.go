package ingest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sentry "github.com/getsentry/sentry-go"
)

// The existing module pins sentry-go v0.47.0. Capture its real transaction
// serialization synchronously before delivery; no global hub or network is used.
// Returning nil here only retains this owned parser fixture, not an ingest receipt.
type rootTagFixtureTransport struct{}

func (*rootTagFixtureTransport) Configure(sentry.ClientOptions)        {}
func (*rootTagFixtureTransport) SendEvent(*sentry.Event)               {}
func (*rootTagFixtureTransport) Flush(time.Duration) bool              { return true }
func (*rootTagFixtureTransport) FlushWithContext(context.Context) bool { return true }
func (*rootTagFixtureTransport) Close()                                {}

func sdkRootTagPayload(t *testing.T) ([]byte, string, string, string) {
	t.Helper()
	var payload []byte
	var marshalErr error
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn: "https://fixture@flare.example.invalid/1", Transport: &rootTagFixtureTransport{},
		Environment: "staging", Release: "relay@fixture", EnableTracing: true, TracesSampleRate: 1,
		Integrations: func([]sentry.Integration) []sentry.Integration { return nil },
		BeforeSendTransaction: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			event.MakeSerializationSafe()
			payload, marshalErr = json.Marshal(event)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	hub := sentry.NewHub(client, sentry.NewScope())
	txn := sentry.StartTransaction(sentry.SetHubOnContext(context.Background(), hub), "GET /api/contacts/{id}", sentry.WithOpName("http.server"))
	txn.SetTag("request_id", strings.Repeat("a", 32))
	txn.SetTag("http.method", "GET")
	txn.SetTag("http.route", "/api/contacts/{id}")
	txn.SetTag("http.status_class", "5xx")
	txn.SetData("http.response.status_code", 500)
	txn.Status = sentry.SpanStatusInternalError
	child := txn.StartChild("db.query")
	child.SetTag("child.only", "yes")
	child.SetTag("request_id", strings.Repeat("b", 32))
	child.SetData("row.count", 1)
	child.Status = sentry.SpanStatusOK
	child.Finish()
	txn.Finish()
	if marshalErr != nil || len(payload) == 0 {
		t.Fatal("SDK transaction serialization was not captured", marshalErr)
	}
	return payload, txn.TraceID.String(), txn.SpanID.String(), child.SpanID.String()
}

func rootTagAttributes(t *testing.T, span SpanRecord) map[string]any {
	t.Helper()
	var attrs map[string]any
	if err := json.Unmarshal(span.Attributes, &attrs); err != nil {
		t.Fatal("span attributes missing or invalid", err)
	}
	return attrs
}

func TestSentryGo047RootTransactionTagsAndCorrelation(t *testing.T) {
	payload, traceID, spanID, childID := sdkRootTagPayload(t)
	var wire struct {
		Tags     map[string]string `json:"tags"`
		Contexts struct {
			Trace struct {
				Tags map[string]string `json:"tags"`
			} `json:"trace"`
		} `json:"contexts"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil || wire.Tags["request_id"] != strings.Repeat("a", 32) || len(wire.Contexts.Trace.Tags) != 0 {
		t.Fatal("fixture no longer exercises SDK top-level transaction tags", err)
	}
	spans, err := ParseSentryTransaction(payload)
	if err != nil || len(spans) != 2 {
		t.Fatal("expected SDK root and child", err, len(spans))
	}
	root, child := spans[0], spans[1]
	if root.TraceID != traceID || root.SpanID != spanID || root.Kind != "server" || root.Status != "internal_error" || root.Name != "GET /api/contacts/{id}" {
		t.Fatal("root identity, name, kind or status changed")
	}
	attrs := rootTagAttributes(t, root)
	for key, want := range map[string]any{"tag.request_id": strings.Repeat("a", 32), "tag.http.method": "GET", "tag.http.route": "/api/contacts/{id}", "tag.http.status_class": "5xx", "data.http.response.status_code": float64(500), "environment": "staging", "release": "relay@fixture", "op": "http.server"} {
		if attrs[key] != want {
			t.Errorf("root attribute %s = %v, want %v", key, attrs[key], want)
		}
	}
	if child.TraceID != traceID || child.SpanID != childID || child.ParentSpanID != spanID || child.Status != "ok" {
		t.Fatal("child identity, parentage or status changed")
	}
	childAttrs := rootTagAttributes(t, child)
	if childAttrs["tag.request_id"] != strings.Repeat("b", 32) || childAttrs["tag.child.only"] != "yes" || childAttrs["data.row.count"] != float64(1) {
		t.Fatal("child-local tags or data changed")
	}
	for _, key := range []string{"tag.http.method", "tag.http.route", "tag.http.status_class", "environment", "release"} {
		if _, exists := childAttrs[key]; exists {
			t.Errorf("root attribute %s leaked into child", key)
		}
	}
	if _, exists := attrs["tag.child.only"]; exists {
		t.Fatal("child tag leaked into root")
	}

	// Error and native-log trace identities remain a join to the same root.
	event := sentry.NewEvent()
	event.Contexts["trace"] = sentry.Context{"trace_id": traceID, "span_id": spanID}
	event.Tags["request_id"] = strings.Repeat("a", 32)
	event.Environment, event.Release = "staging", "relay@fixture"
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	errEvent, err := ParseEvent(raw)
	if err != nil || errEvent.TraceID != root.TraceID || errEvent.SpanID != root.SpanID || errEvent.Environment != "staging" || errEvent.Release != "relay@fixture" {
		t.Fatal("error trace/release/environment join changed", err)
	}
	raw, err = json.Marshal([]map[string]any{{"trace_id": traceID, "attributes": map[string]any{"request_id": strings.Repeat("a", 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := ParseNativeLogs(raw)
	if err != nil || len(logs) != 1 || logs[0].TraceID != root.TraceID {
		t.Fatal("native log trace join changed", err)
	}
	var logAttrs map[string]any
	if err := json.Unmarshal(logs[0].Attributes, &logAttrs); err != nil || logAttrs["request_id"] != attrs["tag.request_id"] {
		t.Fatal("native log request identity differs from root", err)
	}
}

func TestSentryRootTagsRetainLegacyAndPreferCanonicalEventTags(t *testing.T) {
	for _, tc := range []struct {
		name, top string
		want      string
	}{
		{"legacy-only", "", "legacy"},
		{"canonical-wins", `,"tags":{"shared":"canonical","event.only":"yes"}`, "canonical"},
		{"empty-string-is-valid", `,"tags":{"shared":"","event.only":"yes"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"transaction":"fixture","contexts":{"trace":{"trace_id":"trace","span_id":"root","tags":{"shared":"legacy","legacy.only":"yes"}}}` + tc.top + `}`
			spans, err := ParseSentryTransaction([]byte(payload))
			if err != nil || len(spans) != 1 {
				t.Fatal("legacy root rejected", err)
			}
			attrs := rootTagAttributes(t, spans[0])
			if attrs["tag.shared"] != tc.want || attrs["tag.legacy.only"] != "yes" {
				t.Fatal("legacy tag retention or collision precedence changed")
			}
			if tc.top != "" && attrs["tag.event.only"] != "yes" {
				t.Fatal("canonical-only tag missing")
			}
		})
	}
}

func TestSentryMalformedOptionalRootTagsKeepTransactionAndLegacyTags(t *testing.T) {
	for _, tags := range []string{
		`null`, `[]`, `"unexpected"`, `42`,
		`{"shared":"canonical","invalid":42}`,
		`{"shared":null}`,
		`{"event.only":"yes","shared":null}`,
		`{"shared":"canonical","invalid":{"nested":null}}`,
	} {
		payload := `{"tags":` + tags + `,"contexts":{"trace":{"trace_id":"trace","span_id":"root","tags":{"shared":"legacy"}}}}`
		spans, err := ParseSentryTransaction([]byte(payload))
		if err != nil || len(spans) != 1 {
			t.Fatal("malformed optional tags changed transaction acceptance", err)
		}
		attrs := rootTagAttributes(t, spans[0])
		if attrs["tag.shared"] != "legacy" {
			t.Fatal("malformed tags replaced legacy input or admitted a partial map")
		}
		for _, key := range []string{"tag.invalid", "tag.event.only"} {
			if _, exists := attrs[key]; exists {
				t.Fatal("partially decoded invalid tags admitted")
			}
		}
	}
}

func TestSentryRootTransactionTagAttributesKeepSixteenKiBBound(t *testing.T) {
	const empty = `{"tag.large":""}`
	for _, size := range []int{maxSpanAttrs - 1, maxSpanAttrs, maxSpanAttrs + 1} {
		raw, err := json.Marshal(map[string]any{"tags": map[string]string{"large": strings.Repeat("x", size-len(empty))}, "contexts": map[string]any{"trace": map[string]string{"trace_id": "trace", "span_id": "root"}}})
		if err != nil {
			t.Fatal(err)
		}
		spans, err := ParseSentryTransaction(raw)
		if err != nil || len(spans) != 1 {
			t.Fatal("bounded attributes changed span acceptance", err)
		}
		if size <= maxSpanAttrs && len(spans[0].Attributes) != size {
			t.Errorf("attributes size %d = %d", size, len(spans[0].Attributes))
		}
		if size > maxSpanAttrs && spans[0].Attributes != nil {
			t.Fatal("oversized canonical tags bypassed attribute bound")
		}
	}
	// Neither input map alone exceeds the cap; their merged attributes do.
	raw, err := json.Marshal(map[string]any{"tags": map[string]string{"event": strings.Repeat("x", maxSpanAttrs/2)}, "contexts": map[string]any{"trace": map[string]any{"trace_id": "trace", "span_id": "root", "tags": map[string]string{"legacy": strings.Repeat("x", maxSpanAttrs/2)}}}})
	if err != nil {
		t.Fatal(err)
	}
	spans, err := ParseSentryTransaction(raw)
	if err != nil || len(spans) != 1 || spans[0].Attributes != nil {
		t.Fatal("merged tags bypassed existing attribute bound", err)
	}
}
