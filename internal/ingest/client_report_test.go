package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Fixtures follow Sentry Client Reports spec 1.25.0, sentry-go 0.47.0's
// report.ClientReport and sentry-dart 9.21.0's ClientReport.toJson. Numeric
// timestamps are also emitted by sentry-javascript createClientReportEnvelope.
func TestSentryClientReportSDKWireForms(t *testing.T) {
	for _, timestamp := range []string{`"2025-02-07T14:16:00.123Z"`, `1738937760.123`, `null`, ``} {
		t.Run(timestamp, func(t *testing.T) {
			prefix := ""
			if timestamp != "" {
				prefix = `"timestamp":` + timestamp + `,`
			}
			before := time.Now().Add(-time.Second)
			r, err := ParseSentryClientReport([]byte(`{` + prefix + `"discarded_events":[{"reason":"sample_rate","category":"transaction","quantity":12},{"reason":"network_error","category":"log_byte","quantity":1024}]}`))
			if err != nil || len(r.Discards) != 2 {
				t.Fatalf("parse SDK report = %#v, %v", r, err)
			}
			if timestamp == "null" || timestamp == "" {
				if r.ReportedAt.Before(before) || r.ReportedAt.After(time.Now()) {
					t.Fatalf("omitted clock did not use receiver UTC: %s", r.ReportedAt)
				}
			} else if r.ReportedAt.Unix() != 1738937760 {
				t.Fatalf("parsed clock = %s", r.ReportedAt)
			}
		})
	}
	if r, err := ParseSentryClientReport([]byte(`{"discarded_events":[]}`)); err != nil || len(r.Discards) != 0 {
		t.Fatalf("valid empty report = %#v, %v", r, err)
	}
}

func TestSentryClientReportNormalizesWithoutRetainingSenderLabels(t *testing.T) {
	r, err := ParseSentryClientReport([]byte(`{"timestamp":1738937760,"secret":"must-not-be-retained","discarded_events":[{"reason":"future_reason","category":"future_category","quantity":2},{"reason":"different_future_reason","category":"different_category","quantity":3},{"reason":"sample_rate","category":"span","quantity":4},{"reason":"sample_rate","category":"span","quantity":6}]}`))
	if err != nil || len(r.Discards) != 2 {
		t.Fatalf("normalize report = %#v, %v", r, err)
	}
	if r.Discards[0] != (ClientDiscard{Reason: "other", Category: "other", Quantity: 5}) || r.Discards[1].Quantity != 10 {
		t.Fatalf("normalized counters = %#v", r.Discards)
	}
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), "future") || strings.Contains(string(encoded), "secret") {
		t.Fatalf("raw sender dimension retained: %s", encoded)
	}
}

func TestSentryClientReportClockBoundsDoNotDependOnReceiverClock(t *testing.T) {
	future := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name, timestamp string
		want            time.Time
	}{
		{"Unix epoch", `0`, time.Unix(0, 0).UTC()},
		{"RFC3339 epoch", `"1970-01-01T00:00:00Z"`, time.Unix(0, 0).UTC()},
		{"Unix before 2000", `915148800.125`, time.Date(1999, 1, 1, 0, 0, 0, 125000000, time.UTC)},
		{"RFC3339 before 2000", `"1999-01-01T00:00:00Z"`, time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"Unix skewed future", fmt.Sprint(future.Unix()), future},
		{"RFC3339 skewed future", fmt.Sprintf(`"%s"`, future.Format(time.RFC3339Nano)), future},
		{"Unix final fractional second", `253402300799.5`, time.Date(9999, 12, 31, 23, 59, 59, 500000000, time.UTC)},
		{"RFC3339 final nanosecond", `"9999-12-31T23:59:59.999999999Z"`, time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"timestamp":` + tc.timestamp + `,"discarded_events":[{"reason":"network_error","category":"error","quantity":1}]}`
			report, err := ParseSentryClientReport([]byte(payload))
			if err != nil || !report.ReportedAt.Equal(tc.want) || report.ReportedAt.Location() != time.UTC {
				t.Fatalf("reported_at = %s, err = %v; want UTC %s", report.ReportedAt, err, tc.want)
			}
		})
	}
}

func TestSentryClientReportRejectsMalformedAndUnboundedCounters(t *testing.T) {
	valid := `{"reason":"network_error","category":"error","quantity":1}`
	rows := strings.TrimSuffix(strings.Repeat(valid+",", MaxClientReportEntries+1), ",")
	for name, payload := range map[string]string{
		"missing counters":  `{}`,
		"null counters":     `{"discarded_events":null}`,
		"bad JSON":          `{`,
		"bad timestamp":     `{"timestamp":"not-a-date","discarded_events":[]}`,
		"pre-epoch Unix":    `{"timestamp":-1,"discarded_events":[]}`,
		"pre-epoch RFC3339": `{"timestamp":"1969-12-31T23:59:59Z","discarded_events":[]}`,
		"offset pre-epoch":  `{"timestamp":"1970-01-01T00:00:00+01:00","discarded_events":[]}`,
		"Unix year 10000":   `{"timestamp":253402300800,"discarded_events":[]}`,
		"offset year 10000": `{"timestamp":"9999-12-31T23:59:59-01:00","discarded_events":[]}`,
		"numeric overflow":  `{"timestamp":1e309,"discarded_events":[]}`,
		"NaN timestamp":     `{"timestamp":NaN,"discarded_events":[]}`,
		"infinite clock":    `{"timestamp":Infinity,"discarded_events":[]}`,
		"boolean clock":     `{"timestamp":true,"discarded_events":[]}`,
		"object clock":      `{"timestamp":{},"discarded_events":[]}`,
		"too many rows":     `{"discarded_events":[` + rows + `]}`,
		"overlong bytes":    strings.Repeat(" ", MaxClientReportBytes+1),
		"negative":          `{"discarded_events":[{"reason":"sample_rate","category":"span","quantity":-1}]}`,
		"zero":              `{"discarded_events":[{"reason":"sample_rate","category":"span","quantity":0}]}`,
		"fraction":          `{"discarded_events":[{"reason":"sample_rate","category":"span","quantity":1.2}]}`,
		"string quantity":   `{"discarded_events":[{"reason":"sample_rate","category":"span","quantity":"2"}]}`,
		"quantity overflow": `{"discarded_events":[{"reason":"sample_rate","category":"span","quantity":9223372036854775808}]}`,
		"quantity cap":      `{"discarded_events":[{"reason":"sample_rate","category":"span","quantity":1000000001}]}`,
		"sum cap":           `{"discarded_events":[{"reason":"sample_rate","category":"span","quantity":1000000000},{"reason":"sample_rate","category":"transaction","quantity":1}]}`,
		"free text reason":  `{"discarded_events":[{"reason":"email=person@example.com","category":"error","quantity":1}]}`,
		"empty category":    `{"discarded_events":[{"reason":"network_error","category":"","quantity":1}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSentryClientReport([]byte(payload)); err == nil {
				t.Fatalf("accepted malformed report: %s", name)
			}
		})
	}
}
