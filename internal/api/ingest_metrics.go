package api

import (
	"github.com/bright-interaction/flare/internal/metrics"
)

// ingestOutcome is the bounded, logical-signal result of one ingest request.
// Counts describe work that the receiver either persisted, rejected, dropped
// at a configured cap, or failed to persist. It deliberately contains no
// tenant or project identifiers: the private /metrics endpoint is process
// scoped and must remain cardinality-safe.
type ingestOutcome struct {
	accepted uint64
	invalid  uint64
	dropped  uint64
	failed   uint64
}

func (o *ingestOutcome) add(other ingestOutcome) {
	o.accepted += other.accepted
	o.invalid += other.invalid
	o.dropped += other.dropped
	o.failed += other.failed
}

func (o ingestOutcome) record(signal string) {
	signal = canonicalIngestSignal(signal)
	metrics.Add("flare_ingest_items", map[string]string{"signal": signal, "outcome": "accepted"}, o.accepted)
	metrics.Add("flare_ingest_items", map[string]string{"signal": signal, "outcome": "invalid"}, o.invalid)
	metrics.Add("flare_ingest_items", map[string]string{"signal": signal, "outcome": "dropped"}, o.dropped)
	metrics.Add("flare_ingest_items", map[string]string{"signal": signal, "outcome": "failed"}, o.failed)
}

// recordIngestRequest records exactly one request outcome. forced is used for
// failures before a payload can be classified (authentication, body framing,
// or parser errors); a blank value derives the outcome from item counts.
func recordIngestRequest(transport string, o ingestOutcome, forced string) {
	outcome := forced
	if outcome == "" {
		outcome = classifyIngestRequest(o)
	}
	metrics.Inc("flare_ingest_requests", map[string]string{
		"transport": canonicalIngestTransport(transport),
		"outcome":   canonicalIngestOutcome(outcome),
	})
}

func classifyIngestRequest(o ingestOutcome) string {
	if o.failed > 0 && o.accepted == 0 {
		return "failed"
	}
	if o.accepted == 0 {
		if o.invalid > 0 || o.dropped > 0 {
			return "rejected"
		}
		return "empty"
	}
	if o.invalid > 0 || o.dropped > 0 || o.failed > 0 {
		return "partial"
	}
	return "accepted"
}

func canonicalIngestSignal(signal string) string {
	switch signal {
	case "event", "transaction", "log", "span", "metric", "connector", "symbol":
		return signal
	default:
		return "other"
	}
}

func canonicalIngestTransport(transport string) string {
	switch transport {
	case "sentry", "otlp", "native":
		return transport
	default:
		return "other"
	}
}

func canonicalIngestOutcome(outcome string) string {
	switch outcome {
	case "accepted", "partial", "rejected", "failed", "empty":
		return outcome
	default:
		return "failed"
	}
}
