package api

import (
	"fmt"
	"log/slog"
	"strconv"
)

// maxIngestRecords caps the ROWS one ingest request may write, across every
// persist call that request makes.
//
// maxEnvelopeItems (ingest_handlers.go) bounds the ITEMS in a Sentry envelope,
// and on the events path one item is one row, so events were covered by it.
// Every other pillar fans one item out to an unbounded number of rows: an OTLP
// export nests resource -> scope -> record, and a Sentry transaction item
// carries its whole span tree. The byte caps bound the payload; nothing bounded
// the rows.
//
// Measured against the real parsers and scaled to maxDecompressedBody, one 8 MiB
// body encodes 2,796,066 log rows or spans, or 559,191 metric points. The rate
// limiter charges per REQUEST at INGEST_RATE_PER_MIN (1200 by default) and the
// DSN public key ships in the browser bundle of every project, so that ratio was
// reachable with no account.
//
// 10000 sits above every producer that actually ships (the OTel collector's
// batch processor defaults to send_batch_size 8192; Sentry caps a transaction at
// 1000 spans; the native SDK path flushes far less) and roughly 280x under what
// one body can encode.
const maxIngestRecords = 10000

// ingestBudget is the per-REQUEST row budget. One is created per ingest request
// and threaded through every persist call it makes, because handleEnvelope calls
// persistSpans once per transaction item: a cap applied per CALL would still let
// one request write maxEnvelopeItems x cap rows.
type ingestBudget struct {
	left    int
	dropped int
	invalid int
}

func newIngestBudget() *ingestBudget { return &ingestBudget{left: maxIngestRecords} }

// take reserves up to n rows and returns how many this request may still write.
// Anything above the remaining budget is counted as dropped and never written.
//
// It charges for records the caller then discards for other reasons (a NaN
// metric point, say). That is deliberate: the budget bounds the WORK one request
// buys, and parsing a record is most of that work.
func (b *ingestBudget) take(n int) int {
	if n <= b.left {
		b.left -= n
		return n
	}
	allowed := b.left
	b.left = 0
	b.dropped += n - allowed
	return allowed
}

// rejectInvalid records a parsed logical item that cannot be persisted.
// Keeping it on the same request budget makes partial-success responses and
// ingest outcome metrics agree about what the sender must retry or discard.
func (b *ingestBudget) rejectInvalid() { b.invalid++ }

func (b *ingestBudget) rejected() int { return b.dropped + b.invalid }

// report logs truncation and invalid values once per request. Dropping telemetry
// silently is the one failure an observability product must not ship: the
// operator would see a hole in their data and have nothing on either side to
// explain it.
func (b *ingestBudget) report(projectID, pillar string) {
	if b.dropped == 0 && b.invalid == 0 {
		return
	}
	slog.Warn("ingest rejected records",
		"project_id", projectID, "pillar", pillar,
		"dropped", b.dropped, "invalid", b.invalid, "cap", maxIngestRecords)
}

// OTLP partial_success field names, one per signal. They are the wire spelling
// from the OTLP/JSON encoding, not free text.
const (
	otlpRejectedLogRecords = "rejectedLogRecords"
	otlpRejectedSpans      = "rejectedSpans"
	otlpRejectedDataPoints = "rejectedDataPoints"
)

// otlpResponse builds the OTLP/HTTP success body, reporting a partial success
// when rows were dropped.
//
// Without partial_success a collector counts every dropped row as delivered and
// its own sent/failed metrics lie, which is exactly the silent loss the cap must
// not introduce. An empty body when nothing was dropped: partial_success
// present-but-zero reads as "some records failed" to some collectors.
//
// int64 fields are STRINGS in OTLP/JSON, per the proto3 JSON mapping.
func otlpResponse(rejected int, field string) map[string]any {
	if rejected == 0 {
		return map[string]any{}
	}
	return map[string]any{"partialSuccess": map[string]any{
		field: strconv.Itoa(rejected),
		"errorMessage": fmt.Sprintf(
			"%d records rejected: invalid values are dropped and one request may carry at most %d", rejected, maxIngestRecords),
	}}
}
