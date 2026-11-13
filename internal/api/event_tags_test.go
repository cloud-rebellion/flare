package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/ingest"
	"github.com/bright-interaction/flare/internal/scan"
	"github.com/bright-interaction/flare/internal/telemetry"
)

func TestEventTagsAreBoundedSortedStringsOnly(t *testing.T) {
	tests := []struct {
		name, payload string
		want          []eventTag
	}{
		{"sorted", `{"tags":{"route":"signer","category":"pdf_view_error"}}`, []eventTag{{Key: "category", Value: "pdf_view_error"}, {Key: "route", Value: "signer"}}},
		{"no payload", ``, nil},
		{"no tags", `{"message":"boom"}`, nil},
		{"array form", `{"tags":[["route","signer"]]}`, nil},
		{"malformed", `{"tags":`, nil},
		{"non strings dropped", `{"tags":{"a":1,"b":{"x":"y"},"c":null,"d":"kept"}}`, []eventTag{{Key: "d", Value: "kept"}}},
		{"oversized dropped", `{"tags":{"` + strings.Repeat("k", maxEventTagKeyBytes+1) + `":"v","long":"` + strings.Repeat("v", maxEventTagValBytes+1) + `","ok":"v"}}`, []eventTag{{Key: "ok", Value: "v"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eventTags(json.RawMessage(tt.payload))
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}

	many := map[string]string{}
	for i := 0; i < maxEventTags*2; i++ {
		many["k"+strconv.Itoa(i)] = "v"
	}
	raw, _ := json.Marshal(map[string]any{"tags": many})
	if got := eventTags(raw); len(got) != maxEventTags {
		t.Fatalf("tag count = %d, want the %d cap", len(got), maxEventTags)
	}
}

func TestEventTagsAreExposedAndScrubbedAtTheMCPBoundary(t *testing.T) {
	events := []telemetry.Event{{
		ID: "ev1", ReceivedAt: time.Now(),
		Payload: json.RawMessage(`{"tags":{"route":"signer","leak":"` + leakedJWT + `"}}`),
	}}
	s := &Server{}
	out := s.toEventResponses(context.Background(), "iss1", "org1", events, false)
	if len(out) != 1 || len(out[0].Tags) != 2 || out[0].Tags[1] != (eventTag{Key: "route", Value: "signer"}) {
		t.Fatalf("REST event lost its tags: %+v", out)
	}
	scrubbed := scrubEventsForMCP(out)
	if strings.Contains(mcpJSON(scrubbed), leakedJWT) {
		t.Fatalf("a tag crossed the LLM boundary unscrubbed: %s", mcpJSON(scrubbed))
	}
	if scrubbed[0].Tags[1] != (eventTag{Key: "route", Value: "signer"}) {
		t.Fatalf("scrubbing changed a harmless tag: %+v", scrubbed[0].Tags)
	}
}

func tagPayload(t *testing.T, tags map[string]string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"message": "clean", "tags": tags})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func restTags(t *testing.T, tags map[string]string, flagged bool) []eventTag {
	t.Helper()
	events := []telemetry.Event{{ID: "ev1", ReceivedAt: time.Now(), Payload: tagPayload(t, tags)}}
	return (&Server{}).toEventResponses(context.Background(), "iss1", "org1", events, flagged)[0].Tags
}

// F2. A stored event never went through the detector for its tags, so the read
// has to scrub whether or not the issue carries the flag.
func TestTagSecretsAreNeverServedRawByREST(t *testing.T) {
	secrets := map[string]string{
		"session": leakedJWT,
		"gh":      "ghp_ABCDEFGHIJKLMNOPqrstuvwxyz012345",
		"pan":     "4111 1111 1111 1111",
		"url":     "postgres://app:hunter2hunter2@db.internal/app",
	}
	for _, flagged := range []bool{false, true} {
		body, err := json.Marshal(restTags(t, secrets, flagged))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{leakedJWT, secrets["gh"], secrets["pan"], "hunter2hunter2"} {
			if strings.Contains(string(body), secret) {
				t.Errorf("flagged=%v: REST served %q raw: %s", flagged, secret, body)
			}
		}
	}
}

// F2. The flag is what tells the org to rotate, so a tag has to raise the same
// kinds as the same text in the message.
func TestTagSecretsRaiseTheSensitiveFlag(t *testing.T) {
	longJWT := "eyJhbGciOiJIUzI1NiJ9." + strings.Repeat("eyJzdWIiOiJhZG1pbiJ9", 20) + ".sIgNaTuRe1234"
	manyTags := map[string]string{"zz_session": leakedJWT}
	for i := 0; i < maxEventTags*2; i++ {
		manyTags["k"+strconv.Itoa(i)] = "v"
	}
	cases := []struct {
		name    string
		tags    map[string]string
		control string
	}{
		{"jwt value", map[string]string{"session": leakedJWT}, leakedJWT},
		{"secret-named pair", map[string]string{"password": "hunter2hunter2"}, "password=hunter2hunter2"},
		{"secret in key", map[string]string{"ghp_ABCDEFGHIJKLMNOPqrstuvwxyz012345": "seen"}, "ghp_ABCDEFGHIJKLMNOPqrstuvwxyz012345"},
		{"card", map[string]string{"pan": "4111 1111 1111 1111"}, "4111 1111 1111 1111"},
		{"too long to serve", map[string]string{"session": longJWT}, longJWT},
		{"past the serving cap", manyTags, leakedJWT},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := ingest.ParseEvent(tagPayload(t, tt.tags))
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(detectSensitive(parsed), ",")
			want := strings.Join(detectSensitive(ingest.NormalizedEvent{Message: tt.control}), ",")
			if want == "" || got != want {
				t.Fatalf("tag flagged as %q, the same text in the message flags as %q", got, want)
			}
		})
	}

	parsed, err := ingest.ParseEvent(tagPayload(t, map[string]string{"route": "signer", "tokens_used": "1523400", "release": strings.Repeat("a", 40)}))
	if err != nil {
		t.Fatal(err)
	}
	if got := detectSensitive(parsed); got != nil {
		t.Fatalf("clean tags raised the flag: %v", got)
	}
}

// F3 and F6. A pair keeps its key context at both boundaries.
func TestTagPairsAreScrubbedWithTheirKeyContext(t *testing.T) {
	if got := scan.Text("password=hunter2hunter2"); got != "password=[secret]" {
		t.Fatalf("one-string control changed: %q", got)
	}
	cases := []struct{ key, value, wantKey, wantValue string }{
		{"password", "hunter2hunter2", "password", "[secret]"},
		{"session_token", "abcd1234abcd1234", "session_token", "[secret]"},
		{"authorization", "abcd1234abcd1234", "authorization", "[secret]"},
		{"cookie", "sid=abcd1234", "cookie", "[secret]"},
		{"dsn", "abcd1234abcd1234", "dsn", "[secret]"},
		{"DB_PASSWORD", "hunter2hunter2", "DB_PASSWORD", "[secret]"},
		{"clientSecret", "hunter2hunter2", "clientSecret", "[secret]"},
		{"pass\u200bword", "hunter2hunter2", "password", "[secret]"},
		{"x/password", "hunter2hunter2", "x/password", "[secret]"},
		{"password", "hunter2 hunter2", "password", "[secret]"},
		{"tokens_used", "1523400", "tokens_used", "1523400"},
		{"http_route", "/api/v1/{id}", "http_route", "/api/v1/{id}"},
		{"note", "password=hunter2hunter2", "note", "password=[secret]"},
		{"release", strings.Repeat("a", 40), "release", strings.Repeat("a", 40)},
		{"release", leakedJWT, "release", "[jwt]"},
		{"commit", strings.Repeat("a", 40), "commit", "[hash]"},
	}
	for _, tt := range cases {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			want := eventTag{Key: tt.wantKey, Value: tt.wantValue}
			rest := restTags(t, map[string]string{tt.key: tt.value}, false)
			if len(rest) != 1 || rest[0] != want {
				t.Errorf("REST unflagged = %+v, want %+v", rest, want)
			}
			flagged := restTags(t, map[string]string{tt.key: tt.value}, true)
			if len(flagged) != 1 || flagged[0] != want {
				t.Errorf("REST flagged = %+v, want %+v", flagged, want)
			}
			mcp := scrubEventsForMCP([]eventResponse{{ID: "e1", Tags: restTags(t, map[string]string{tt.key: tt.value}, true)}})
			if len(mcp[0].Tags) != 1 || mcp[0].Tags[0] != want {
				t.Errorf("MCP = %+v, want %+v", mcp[0].Tags, want)
			}
		})
	}
}

// F4. Scrubbing can collapse distinct keys, and the dashboard must not be handed
// duplicates at either boundary.
func TestScrubbedTagKeysStayUnique(t *testing.T) {
	inputs := map[string]map[string]string{
		"emails":       {"a@x.se": "1", "b@x.se": "2", "c@x.se": "3"},
		"literal":      {"[email]": "0", "a@x.se": "1", "[email]#2": "2", "b@x.se": "3"},
		"format chars": {"ab": "1", "a\u200bb": "2"},
		"all format":   {"\u200b": "1", "\u200c": "2"},
	}
	for name, tags := range inputs {
		t.Run(name, func(t *testing.T) {
			got := restTags(t, tags, false)
			if len(got) != len(tags) {
				t.Fatalf("a tag was dropped: %+v", got)
			}
			mcp := scrubEventsForMCP([]eventResponse{{ID: "e1", Tags: append([]eventTag(nil), got...)}})
			for _, list := range [][]eventTag{got, mcp[0].Tags} {
				seen := map[string]bool{}
				for _, tag := range list {
					if seen[tag.Key] {
						t.Fatalf("duplicate key %q in %+v", tag.Key, list)
					}
					seen[tag.Key] = true
				}
			}
		})
	}
}

func scannedTags(n int) string {
	long := strings.Repeat("v", 2*maxEventTagScanFieldBytes)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = `"` + strconv.Itoa(i) + long + `":"` + long + `"`
	}
	return strings.Join(parts, ",")
}

// hostileTagPayloads are the shapes a DSN holder can store, at the 8 MiB body cap.
func hostileTagPayloads() map[string][]byte {
	const body = 8 << 20
	var keys strings.Builder
	keys.WriteString(`{"tags":{`)
	for i := 0; keys.Len() < body-32; i++ {
		if i > 0 {
			keys.WriteByte(',')
		}
		keys.WriteString(`"k` + strconv.Itoa(i) + `":"v"`)
	}
	keys.WriteString(`}}`)

	var topLevel strings.Builder
	topLevel.WriteString(`{`)
	for i := 0; topLevel.Len() < body-32; i++ {
		if i > 0 {
			topLevel.WriteByte(',')
		}
		topLevel.WriteString(`"k` + strconv.Itoa(i) + `":1`)
	}
	topLevel.WriteString(`,"tags":{"a":"b"}}`)

	return map[string][]byte{
		"many tag keys":        []byte(keys.String()),
		"many top-level keys":  []byte(topLevel.String()),
		"huge tag value":       []byte(`{"tags":{"a":"` + strings.Repeat("v", body-32) + `"}}`),
		"padding before tags":  []byte(`{"a":"` + strings.Repeat("v", body-32) + `","tags":{"a":"b"}}`),
		"longest scanned tags": []byte(`{"tags":{` + scannedTags(maxEventTagScanEntries) + `}}`),
		"many small tags":      []byte(`{"tags":{` + strings.Repeat(`"k":"v",`, (maxEventTagScanBytesForTest-16)/8) + `"k":"v"}}`),
	}
}

// maxEventTagScanBytesForTest is the size of the largest document the read path
// may have to walk completely.
const maxEventTagScanBytesForTest = 256 << 10

// F5. A read walks 50 of these per request, so the work per event must not grow
// with the stored document. Allocations rather than wall time, so the test holds
// on a loaded machine; BenchmarkEventTagsHostile reports the time.
func TestEventTagsWorkIsBoundedOnHostilePayloads(t *testing.T) {
	const maxAllocs = 5000
	for name, payload := range hostileTagPayloads() {
		t.Run(name, func(t *testing.T) {
			allocs := testing.AllocsPerRun(1, func() { eventTags(payload) })
			if allocs > maxAllocs {
				t.Fatalf("eventTags made %.0f allocations on a %d byte payload, budget %d", allocs, len(payload), maxAllocs)
			}
		})
	}
}

// F5. The byte window is what bounds a document that costs CPU but few
// allocations, so pin that it applies, and that the tags of an ordinary large
// event inside it still come through.
func TestEventTagsReadOnlyTheScanWindow(t *testing.T) {
	pad := func(n int) string { return `{"a":"` + strings.Repeat("v", n) + `","tags":{"route":"signer"}}` }
	if got := eventTags([]byte(pad(maxEventTagScanBytesForTest))); got != nil {
		t.Fatalf("read past the window: %+v", got)
	}
	got := eventTags([]byte(pad(maxEventTagScanBytesForTest - 64)))
	if len(got) != 1 || got[0] != (eventTag{Key: "route", Value: "signer"}) {
		t.Fatalf("lost a tag inside the window: %+v", got)
	}
}

// F5. Detection runs regexes over this text inside the ingest transaction, so the
// tags' share of it has a budget too.
func TestDetectorTagTextIsBounded(t *testing.T) {
	for name, payload := range hostileTagPayloads() {
		t.Run(name, func(t *testing.T) {
			if got := len(sensitiveText(ingest.NormalizedEvent{Raw: payload})); got > 128<<10 {
				t.Fatalf("detector reads %d bytes of tag text", got)
			}
		})
	}
}

// BenchmarkEventTagsHostile measures one issue read: 50 hostile events.
func BenchmarkEventTagsHostile(b *testing.B) {
	for name, payload := range hostileTagPayloads() {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for range 50 {
					eventTags(payload)
				}
			}
		})
	}
}

// BenchmarkDetectSensitiveHostileTags measures the ingest side, per event.
func BenchmarkDetectSensitiveHostileTags(b *testing.B) {
	for name, payload := range hostileTagPayloads() {
		parsed := ingest.NormalizedEvent{Raw: payload}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				detectSensitive(parsed)
			}
		})
	}
}
