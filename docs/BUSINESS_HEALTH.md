# Business health checks

Flare accepts a bounded business outcome from a trusted backend or SaaS
adapter using a **confidential project business key**. Provision it with an
admin session at `POST /api/projects/<project-id>/business-keys`:

```json
{"name":"production backend","expires_in_days":90}
```

The `201` response shows the `flb_...` key once. Store it in the backend's
secret manager. `GET /api/projects/<project-id>/business-keys` lists only ID,
name, prefix, creation, expiry, and revocation timestamps. To rotate, create a
new key, switch the backend, verify an accepted check, then call
`DELETE /api/projects/<project-id>/business-keys/<key-id>` for the old key.
Creation and revocation are admin-only and audited. The default lifetime is
90 days; the maximum is 365 days. A project can have at most eight active,
unexpired keys. The list shows active keys before retired history, so every
usable key stays visible even when more than 100 old keys exist. Keep one spare
active-key slot for create-new-before-revoke rotation; a `409` at the cap means
an unused key must be revoked before another can be issued.

Send a business outcome from the backend:

```sh
curl -X POST "https://flare.example/api/<project-id>/business-checks" \
  -H "Authorization: Bearer <business-check-key>" \
  -H "Idempotency-Key: <unique-operation-id>" \
  -H "Content-Type: application/json" \
  -d '{"check_name":"checkout.completed","outcome":"error","elapsed_ms":842,"status_class":"5xx","error_code":"payment_declined"}'
```

The public browser/Flutter DSN cannot authorize this endpoint, even when
present alongside a business key. A web or Flutter client should report its
operation to its own backend, which validates the result and sends the bounded
outcome to Flare. Do not embed the business key in browser or mobile code.
Flare rate limits business submissions by both source IP and a hash of the
business key using `INGEST_RATE_PER_MIN`; several services behind one shared
NAT share the IP allowance. Size `INGEST_RATE_PER_MIN` for their combined peak
request volume and watch for `429` responses; the limit is applied per Flare
instance rather than coordinated across replicas.
Authenticated submissions are counted in one-minute, payload-free usage
buckets under `business_key` / `business_checks`, scoped to the project and
opaque key ID. An idempotent retry counts as another request, while the
business outcome counts once. Each authenticated response other than `202`
also increments `errors`; a `401` after authentication, when a concurrent
revocation wins, increments both `errors` and `denied`. Use `GET /api/usage` or
a project `usage_threshold` rule to monitor requests, errors, or denied calls.
Missing, unknown, expired, revoked, wrong-project, and rate-limited keys do not
reach authenticated usage accounting; ingest rejection metrics cover attempts
that reach the handler.

`check_name` is a stable lowercase token (for example,
`checkout.completed`, `payment.authorized`, or `account.provisioned`). The
request contains only the outcome, elapsed milliseconds, HTTP status class, and
a bounded error code. Do not send URLs, request bodies, customer identifiers,
credentials, or provider response text. Flare derives timestamps and tenant
scope from the confidential project's key.

`elapsed_ms` and `Idempotency-Key` are required. Generate one opaque 16–128
character key for each business operation using letters, digits, `.`, `_`, `~`,
or `-`. Reuse that exact key and the same assertion when retrying after a
timeout, connection loss, or `503`. Flare stores only a SHA-256 hash of the key,
unique within the authenticated organization and project. An identical retry
returns the original receipt with `Idempotency-Replayed: true` and does not
add another business result. Reusing the key with a changed normalized
assertion returns `409`; send a new key only for a genuinely new operation.

The result and its payload-free ingest receipt are committed together. A `202`
response includes `accepted: true` and a `receipt_id`. A `503` means durable
acceptance could not be confirmed; retry with the same key, even if the first
commit may have succeeded. The receipt can be viewed with the project's
ingest-receipts endpoint.

Authenticated viewers can query aggregate business health:

```text
GET /api/projects/<project-id>/business-health?hours=24&limit=100
```

Rows contain attempts, successful and failed counts, error rate, latency
aggregates, and last observation timestamps. They are scoped by both project
and organization and contain no business payload.

To alert, create a `business_health` rule with an exact `business_check_name`,
an error-rate `threshold_value` from `0` to `1`, and a `window_minutes` value
from `1` to `720`. A threshold of `0` fires on any observed error and stays
quiet for an all-success series. The watchdog stays quiet when no observations
exist, so a separate service freshness or check-in rule should cover missing
producers.
