package ingest

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// maxSpanAttrs caps a single span's serialized attributes so a pathological
// transaction cannot write unbounded JSON.
const maxSpanAttrs = 16 << 10

// errNoTraceContext means the transaction item had no usable trace context
// (no trace_id / span_id), so there is nothing to store.
var errNoTraceContext = errors.New("sentry transaction: missing trace context")

// sentryTime unmarshals a Sentry timestamp, which SDKs send either as a unix
// seconds float (with fractional part) or an RFC3339 string.
type sentryTime struct{ time.Time }

func (t *sentryTime) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] != '"' { // numeric: unix seconds, optionally fractional
		// Parse as a decimal string rather than float64: current unix timestamps
		// exceed float64's exact-integer range, so f - float64(sec) loses sub-ms
		// precision (catastrophic cancellation). Splitting on the dot keeps the
		// seconds exact and gives full-nanosecond fractional precision.
		intPart, fracPart, _ := strings.Cut(s, ".")
		sec, err := strconv.ParseInt(intPart, 10, 64)
		if err != nil {
			return err
		}
		var nsec int64
		if fracPart != "" {
			if len(fracPart) > 9 {
				fracPart = fracPart[:9]
			}
			for len(fracPart) < 9 {
				fracPart += "0"
			}
			if nsec, err = strconv.ParseInt(fracPart, 10, 64); err != nil {
				return err
			}
			if sec < 0 {
				nsec = -nsec
			}
		}
		t.Time = time.Unix(sec, nsec).UTC()
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.Trim(s, `"`))
	if err != nil {
		return err
	}
	t.Time = parsed.UTC()
	return nil
}

// sentrySpanCommon is the trace context shared by the root (contexts.trace) and
// each child span in a Sentry transaction.
type sentrySpanCommon struct {
	TraceID      string            `json:"trace_id"`
	SpanID       string            `json:"span_id"`
	ParentSpanID string            `json:"parent_span_id"`
	Op           string            `json:"op"`
	Description  string            `json:"description"`
	Status       string            `json:"status"`
	Tags         map[string]string `json:"tags"`
	Data         map[string]any    `json:"data"`
}

type sentryChildSpan struct {
	sentrySpanCommon
	StartTS   sentryTime `json:"start_timestamp"`
	Timestamp sentryTime `json:"timestamp"`
}

type sentryTransaction struct {
	Tags        json.RawMessage `json:"tags"`
	Transaction string          `json:"transaction"`
	StartTS     sentryTime      `json:"start_timestamp"`
	Timestamp   sentryTime      `json:"timestamp"`
	Contexts    struct {
		Trace sentrySpanCommon `json:"trace"`
	} `json:"contexts"`
	Spans       []sentryChildSpan `json:"spans"`
	Environment string            `json:"environment"`
	Release     string            `json:"release"`
}

// ParseSentryTransaction maps a Sentry "transaction" envelope item to Flare
// span records: the transaction itself is the root span (from contexts.trace +
// the top-level start/end timestamps) and each entry in spans[] is a child.
func ParseSentryTransaction(payload []byte) ([]SpanRecord, error) {
	var tx sentryTransaction
	if err := json.Unmarshal(payload, &tx); err != nil {
		return nil, err
	}
	tr := tx.Contexts.Trace
	if tr.TraceID == "" || tr.SpanID == "" {
		return nil, errNoTraceContext
	}

	out := make([]SpanRecord, 0, len(tx.Spans)+1)

	// start_time is the partition key and is fully client-supplied, so it is
	// clamped into the partitioned window. DurationMs still comes from the raw
	// pair below, so a clamp never distorts the reported latency.
	now := time.Now().UTC()

	// Sentry SDK transaction tags live at the event's top level. Keep legacy
	// trace-context tags, with canonical event tags winning any duplicate key.
	// Malformed optional tags were previously ignored, so do not newly reject
	// an otherwise usable transaction or admit a partially decoded tag map.
	// Pointer values distinguish JSON null from a valid empty string.
	var eventTagValues map[string]*string
	var eventTags map[string]string
	if json.Unmarshal(tx.Tags, &eventTagValues) == nil {
		eventTags = make(map[string]string, len(eventTagValues))
		for key, value := range eventTagValues {
			if value == nil {
				eventTags = nil
				break
			}
			eventTags[key] = *value
		}
	}
	// Only the root receives these tags; each child retains its own local tags.
	rootTags := make(map[string]string, len(tr.Tags)+len(eventTags))
	for key, value := range tr.Tags {
		rootTags[key] = value
	}
	for key, value := range eventTags {
		rootTags[key] = value
	}
	rootName := firstNonEmpty(tx.Transaction, tr.Description, tr.Op)
	out = append(out, SpanRecord{
		TraceID:      tr.TraceID,
		SpanID:       tr.SpanID,
		ParentSpanID: tr.ParentSpanID,
		Name:         rootName,
		Kind:         opKind(tr.Op),
		Status:       normSpanStatus(tr.Status),
		Start:        ClampTime(tx.StartTS.Time, now),
		End:          ClampTime(tx.Timestamp.Time, now),
		DurationMs:   durationMs(tx.StartTS.Time, tx.Timestamp.Time),
		Attributes:   spanAttrs(tr.Op, tr.Description, rootTags, tr.Data, tx.Environment, tx.Release),
	})

	for _, sp := range tx.Spans {
		if sp.SpanID == "" {
			continue
		}
		out = append(out, SpanRecord{
			TraceID:      firstNonEmpty(sp.TraceID, tr.TraceID),
			SpanID:       sp.SpanID,
			ParentSpanID: firstNonEmpty(sp.ParentSpanID, tr.SpanID),
			Name:         firstNonEmpty(sp.Description, sp.Op),
			Kind:         opKind(sp.Op),
			Status:       normSpanStatus(sp.Status),
			Start:        ClampTime(sp.StartTS.Time, now),
			End:          ClampTime(sp.Timestamp.Time, now),
			DurationMs:   durationMs(sp.StartTS.Time, sp.Timestamp.Time),
			Attributes:   spanAttrs(sp.Op, sp.Description, sp.Tags, sp.Data, "", ""),
		})
	}
	return out, nil
}

// opKind maps a Sentry span op to an OTLP-style span kind so the trace UI reads
// consistently across the OTLP and Sentry-wire ingest paths.
func opKind(op string) string {
	switch {
	case op == "":
		return "internal"
	case strings.Contains(op, "server"):
		return "server"
	case strings.HasPrefix(op, "http.client"), strings.HasPrefix(op, "db"), strings.HasPrefix(op, "cache"), strings.Contains(op, "client"):
		return "client"
	default:
		return "internal"
	}
}

func normSpanStatus(s string) string {
	if s == "" {
		return "unset"
	}
	return strings.ToLower(s)
}

func durationMs(start, end time.Time) float64 {
	if end.After(start) {
		return float64(end.Sub(start).Microseconds()) / 1000.0
	}
	return 0
}

func spanAttrs(op, desc string, tags map[string]string, data map[string]any, env, release string) json.RawMessage {
	m := map[string]any{}
	if op != "" {
		m["op"] = op
	}
	if desc != "" {
		m["description"] = desc
	}
	for k, v := range tags {
		m["tag."+k] = v
	}
	for k, v := range data {
		m["data."+k] = v
	}
	if env != "" {
		m["environment"] = env
	}
	if release != "" {
		m["release"] = release
	}
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil || len(b) > maxSpanAttrs {
		return nil
	}
	return b
}
