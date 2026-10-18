package ingest

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"time"
)

const (
	MaxClientReportBytes   = 4 << 10
	MaxClientReportEntries = 64
	MaxClientReportCount   = int64(1_000_000_000)
)

// ClientReport contains only normalized SDK assertions. It is not evidence
// that Flare observed a server failure, and contains no raw event or report.
type ClientReport struct {
	ReportedAt time.Time
	Discards   []ClientDiscard
}

type ClientDiscard struct {
	Reason   string
	Category string
	Quantity int64
}

// ParseSentryClientReport accepts the official SDKs' RFC3339 or Unix-second
// timestamp encodings. Unknown, syntactically safe labels become "other" so
// arbitrary sender strings never become stored dimensions. Unknown JSON fields
// are ignored for SDK compatibility and are never retained.
func ParseSentryClientReport(payload []byte) (ClientReport, error) {
	var result ClientReport
	if len(payload) == 0 || len(payload) > MaxClientReportBytes {
		return result, errors.New("client report exceeds payload bounds")
	}
	var wire struct {
		Timestamp json.RawMessage `json:"timestamp"`
		Discards  []struct {
			Reason   string `json:"reason"`
			Category string `json:"category"`
			Quantity int64  `json:"quantity"`
		} `json:"discarded_events"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil || wire.Discards == nil || len(wire.Discards) > MaxClientReportEntries {
		return result, errors.New("invalid client report")
	}
	reportedAt, err := clientReportTimestamp(wire.Timestamp)
	if err != nil {
		return result, err
	}
	result.ReportedAt = reportedAt
	// Combine duplicate and normalized unknown dimensions before persistence.
	totals := make(map[[2]string]int64, len(wire.Discards))
	var total int64
	for _, row := range wire.Discards {
		quantity := row.Quantity
		if quantity <= 0 || quantity > MaxClientReportCount || total > MaxClientReportCount-quantity || !clientReportToken(row.Reason) || !clientReportToken(row.Category) {
			return ClientReport{}, errors.New("invalid client report discard")
		}
		total += quantity
		key := [2]string{clientReportReason(row.Reason), clientReportCategory(row.Category)}
		totals[key] += quantity
	}
	for key, quantity := range totals {
		result.Discards = append(result.Discards, ClientDiscard{Reason: key[0], Category: key[1], Quantity: quantity})
	}
	sort.Slice(result.Discards, func(i, j int) bool {
		if result.Discards[i].Reason != result.Discards[j].Reason {
			return result.Discards[i].Reason < result.Discards[j].Reason
		}
		return result.Discards[i].Category < result.Discards[j].Category
	})
	return result, nil
}

func clientReportTimestamp(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Now().UTC(), nil
	}
	var timestamp time.Time
	if len(raw) > 0 && raw[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			timestamp, _ = time.Parse(time.RFC3339Nano, value)
		}
	} else if seconds, err := strconv.ParseFloat(string(raw), 64); err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds >= 0 && seconds < 253402300800 {
		whole, fraction := math.Modf(seconds)
		timestamp = time.Unix(int64(whole), int64(fraction*1e9))
	}
	// Device clocks are untrusted metadata. Fixed UTC storage bounds accept
	// offline and skewed-clock reports without letting their clock control
	// query freshness, which always uses Flare's received_at.
	if timestamp.Before(time.Unix(0, 0)) || !timestamp.Before(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		return time.Time{}, errors.New("invalid client report timestamp")
	}
	return timestamp.UTC(), nil
}

func clientReportToken(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if c < 'a' || c > 'z' {
			if c < '0' || c > '9' {
				if c != '_' {
					return false
				}
			}
		}
	}
	return true
}

func clientReportReason(value string) string {
	switch value {
	case "sample_rate", "span_sample_rate", "before_send", "event_processor", "ignored", "queue_overflow", "buffer_overflow", "cache_overflow", "ratelimit_backoff", "backpressure", "network_error", "send_error", "internal_sdk_error", "shutdown", "insufficient_data":
		return value
	default:
		return "other"
	}
}

func clientReportCategory(value string) string {
	switch value {
	case "error", "transaction", "span", "log", "log_item", "log_byte", "trace_metric", "trace_metric_byte", "monitor", "attachment", "profile", "profile_chunk", "replay", "metric_bucket", "metric", "session", "default", "feedback", "user_report", "security", "unknown", "__all__":
		return value
	default:
		return "other"
	}
}
