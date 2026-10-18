package api

import "encoding/json"

// Codes are owned by the receiver, never copied from an SDK type or parse error.
// The matching database constraint bounds both keys and integer counts. Empty
// historical/other-protocol receipts mean detail unavailable, not zero losses.
func canonicalIngestRejection(code string) string {
	switch code {
	case "invalid_envelope", "invalid_event", "invalid_event_id",
		"invalid_transaction", "invalid_transaction_id", "invalid_client_report",
		"unsupported_session", "unsupported_attachment", "unsupported_profile",
		"unsupported_check_in", "unsupported_log", "unsupported_span",
		"unsupported_metric", "unsupported_other", "identity_conflict", "storage_failed":
		return code
	default:
		return "unsupported_other"
	}
}

func unsupportedSentryReason(itemType string) string {
	switch itemType {
	case "session", "sessions":
		return "unsupported_session"
	case "attachment":
		return "unsupported_attachment"
	case "profile", "profile_chunk":
		return "unsupported_profile"
	case "check_in":
		return "unsupported_check_in"
	case "log", "logs":
		return "unsupported_log"
	case "span":
		return "unsupported_span"
	case "statsd", "metric_buckets":
		return "unsupported_metric"
	default:
		return "unsupported_other"
	}
}

func (r *ingestReceipt) reject(code string, count int) {
	if r == nil || count <= 0 {
		return
	}
	if r.rejections == nil {
		r.rejections = make(map[string]int64)
	}
	code = canonicalIngestRejection(code)
	n := r.rejections[code] + int64(count)
	if n > maxEnvelopeItems {
		n = maxEnvelopeItems
	}
	r.rejections[code] = n
}

func (r *ingestReceipt) rejectionJSON() json.RawMessage {
	if r == nil || len(r.rejections) == 0 {
		return json.RawMessage(`{}`)
	}
	// map[string]int64 always marshals; no user values or custom marshaler.
	b, _ := json.Marshal(r.rejections)
	return b
}

func safeIngestRejections(raw json.RawMessage) map[string]int64 {
	out := make(map[string]int64)
	var counts map[string]int64
	if len(raw) > 2048 || json.Unmarshal(raw, &counts) != nil {
		return out
	}
	for code, count := range counts {
		if canonicalIngestRejection(code) == code && count > 0 && count <= maxEnvelopeItems {
			out[code] = count
		}
	}
	return out
}
