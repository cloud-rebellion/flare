# SDK discard reports

Flare accepts Sentry `client_report` envelope items alongside events and
transactions, or in a report-only envelope. These reports explain telemetry
the SDK says it discarded before delivery. They are client assertions: an
authenticated project DSN identifies their source, but does not prove that a
reported failure happened. Sampling and application filtering can be deliberate.

The wire forms follow [Sentry Client Reports specification 1.25.0](https://develop.sentry.dev/sdk/telemetry/client-reports/),
[sentry-go 0.47.0](https://github.com/getsentry/sentry-go/blob/v0.47.0/report/report.go),
and [sentry-dart 9.21.0](https://github.com/getsentry/sentry-dart/blob/9.21.0/packages/dart/lib/src/client_reports/client_report.dart).
The parser accepts RFC3339 timestamps used by Go and Dart/Flutter, numeric Unix
seconds used by browser SDKs, and an omitted timestamp (receiver UTC time).
This establishes wire compatibility; deployed browser and Flutter journeys
still need live acceptance with their actual SDK versions.

```text
{}
{"type":"client_report"}
{"timestamp":1738937760.123,"discarded_events":[{"reason":"sample_rate","category":"transaction","quantity":12},{"reason":"network_error","category":"error","quantity":3}]}
```

Each report is limited to 4 KiB, 64 input counters and a total quantity of
1,000,000,000 positive integer units. Timestamps must parse to a UTC instant
from 1970 inclusive to year 10000 exclusive. Offline reports and future client
clock skew are accepted within those fixed bounds. The client timestamp is
untrusted metadata; query windows and freshness use Flare's receiver time.
An empty `discarded_events` array is valid; the array is required.
Reasons and categories must be nonempty tokens of at most 64 lowercase ASCII
letters, digits or underscores. Known labels retain their meaning; future safe
labels become `other` so arbitrary sender text is never stored. Extra JSON
fields are ignored and are not retained. Duplicate reason/category counters
within a report are combined.

Dart/Flutter's `ignored` reason is classified as filtering. New valid reason
or category tokens remain accepted through the `other` bucket until their
meaning is reviewed; this avoids breaking SDK upgrades or storing free text.

Only normalized counters, the SDK timestamp, receiver time, tenant/project keys
and the report's SHA-256 identity are stored. The same bounded database
transaction commits these counters, the accepted-item identity and the terminal
ingest receipt. A storage or receipt failure rolls back the accepted data and
returns HTTP 503. Reports share the request's 10,000-row budget with spans;
a report that does not fit is dropped as a whole and its identity is released.
The wire response and receipt make rejected or dropped items explicit.
Counter rows expire with the Sentry accepted-item retention policy through a
foreign-key cascade, and project deletion removes them as well.

Exact report payloads are replay-deduplicated per project, including concurrent
retries. Reports have no stable event ID, so identical bytes from independent
SDK clients can collide. Reserializing a report or changing its timestamp can
also bypass exact-byte deduplication. These are approximate sender counters,
not globally exact lost-event totals. A safe explicit retry does not mean an
SDK automatically retries HTTP 503: the current Sentry protocol permits or
requires discarding on HTTP errors. Check each sender's actual retry behavior.

## Reading the breakdown

`GET /api/projects/{id}/sdk-discards?hours=24` requires the same authenticated
viewer access as other project reads. The window defaults to 24 hours and
accepts integers from 1 to 168. Both organization and project scope are enforced
in the query and database ownership constraints. A foreign project returns 404.

```json
{
  "source_kind": "client_reported",
  "protocol": "sentry",
  "window_hours": 24,
  "window_basis": "received_at",
  "since": "2026-10-17T15:00:00Z",
  "breakdown": [
    {
      "reason": "sample_rate",
      "category": "transaction",
      "kind": "sampling",
      "quantity": "12",
      "reports": 1,
      "first_received_at": "2026-10-18T14:59:00Z",
      "last_received_at": "2026-10-18T14:59:00Z"
    }
  ],
  "interpretation": "SDK assertions; sampling and filtering may be intentional. Quantities use each category's units and are not server-observed failures. Exact report payload replays are counted once per project; identical reports from independent clients can collide."
}
```

`kind` groups known reasons as `sampling`, `filtering`, `buffer`, `rate_limited`,
`delivery` or `sdk_error`; unclassified reasons use `unknown`. The original
known reason remains visible. `other` reason/category buckets retain the count
of unrecognized safe labels without retaining the original strings.

Quantities are decimal strings, preserving PostgreSQL's numeric sum without
overflow or JavaScript number rounding. Category units differ: `log_byte`
counts bytes while `error` counts items, and dropped transactions may also
produce span counters. Do not add quantities across categories into a lost
event total. `reports` counts distinct payload identities for each bucket;
do not add those counts across buckets either. The time window and freshness
use receiver time, so a client's clock cannot hide a newly received report.
An empty breakdown means no accepted counters were observed in that window.

This support does not establish the cause of historical rejected envelopes.
Existing receipts identify the protocol and counts, but lack item-level reject
reasons. Unsupported sessions, attachments and malformed event/transaction
items can still be rejected.
