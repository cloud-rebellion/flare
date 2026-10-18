# Automation webhook channel

Flare can notify an external coding-loop or operations worker when a project
alert is delivered. The `automation_webhook` channel is a notification
boundary only: it does not run code, approve a fix, start CI, deploy a release,
or claim synthetic verification.

## Create a channel

Create it through the authenticated `POST /api/channels` endpoint (or the
Settings page):

```json
{
  "type": "automation_webhook",
  "enabled": false,
  "project_ids": ["project-id"],
  "config": {
    "url": "https://worker.example.com/flare",
    "signing_secret": "a-random-secret-at-least-16-bytes",
    "receipt_mode": "http"
  }
}
```

Create the channel paused and supply its project routing in this same request.
Flare validates every project against the workspace and commits the channel and
all routes together, so a failed route leaves no channel and a concurrent alert
cannot see a temporary all-project channel. Duplicate project IDs are collapsed;
at most 256 distinct IDs of 1–128 ASCII letters, digits, `_` or `-` are accepted.
The returned `enabled` and `project_ids` describe the committed creation state.
For compatibility, omitting `enabled` means `true`, and omitting `project_ids`
or sending an empty array means every workspace project, including future ones.
Explicit `null` is invalid for either creation option. New Settings connections
start paused and require an explicit project selection or all-project choice.

Use `PATCH /api/channels/{id}` with `{"enabled":true}` after testing and
reviewing the receiver and alert rule. That route can also atomically replace
`project_ids`; it does not edit the destination, signing secret or receipt mode.
A workspace can store 25 channels including paused channels. Delete an unused
channel to make room at that cap. A legacy workspace already above the cap can
still pause, route, delete or idempotently enable existing channels, but cannot
create another channel or enable a paused one while 25 are already enabled.

The URL must be a public HTTPS endpoint. Loopback, private, link-local,
metadata, and other blocked destinations are rejected at creation and the
delivery client checks the resolved address again. The signing secret must be
16–256 printable bytes. Both values are encrypted at rest and redacted from
channel responses and delivery errors.

For a reviewed Reactor `flare-v1` trigger, use its provider-bound public HTTPS
`/webhook/flare/{token}` URL and explicitly choose `"receipt_mode":"reactor-v1"`.
The omitted/default `http` mode preserves generic automation receiver
compatibility: any HTTP 2xx is delivery only and asserts no receiver run. An
unknown or null mode is rejected before sending. No existing channel is
automatically upgraded to Reactor receipt mode.

## Durable receiver acknowledgement

Reactor mode requires one `application/json` response of at most 2,048 bytes.
A real notice must return exactly
`{"accepted":true,"deduped":false,"run_id":"run_0123456789abcdef"}`;
`deduped:true` acknowledges the same previously accepted notice. The run ID
must be a nonempty bounded identifier, never a URL. A signed `test` instead
requires exactly `{"accepted":true,"test":true,"dispatched":false}` and must
not assert a run. Unknown/duplicate/null fields, missing run IDs, telemetry,
oversized responses, HTML success pages, redirects and malformed JSON fail the
delivery attempt and follow the existing bounded retry policy. Response bodies,
headers, URLs, credentials and provider/issue text are never stored.

The outbox worker commits only a small acknowledgement together with the
delivered state under the same current, unexpired lease. It binds the receipt
to the original notice's scope, fields and exact signed-body SHA-256. A stale
worker cannot publish or overwrite it. If Flare loses its database connection
after Reactor enqueued a run, the same stable body is retried; Reactor dedupes
it and Flare can commit that run acknowledgement on the new lease.

Authenticated viewers can read an exact original notice at
`GET /api/projects/{id}/automation-deliveries/{noticeID}`. Its
`acknowledgement` is one of:

- `unrecorded`: no receiver proof, including historical deliveries.
- `http_delivery`: HTTP 2xx only; no run or execution assertion.
- `receiver_enqueued`: the configured Reactor receiver acknowledged a run ID.
- `test_noop`: the receiver acknowledged a signed test without dispatch.

Discover notice IDs through
`GET /api/projects/{id}/automation-deliveries?limit=25&issue_id=<optional-id>`.
The response is `{"items":[...],"next_cursor":"..."}` with the same bounded
projection. Pages use newest-first `created_at`/ID ordering; pass the opaque
`next_cursor` back as `cursor` to load older notices. The cursor is bound to
the organization, project and exact issue filter. Changing that scope requires
starting a fresh page. `limit` defaults to 25 and must be 1–100; invalid limits,
oversized filters and malformed/foreign cursors return 400. An empty page has
`"items":[]`; the cursor is omitted when there are no older results.

The response contains bounded notice metadata, HTTP status, request digest,
database acknowledgement time and optional receiver run ID/deduplication flag.
It remains readable after channel deletion and is scoped by organization and
project. Receiver enqueue acknowledgement is separate from fresh AI evidence,
successful workflow execution, human approval, a completed fix, exact CI,
deployment and synthetic verification. Those later receipts are still required.

The v0.6.0 candidate adds this projection in migration
`089_automation_delivery_acknowledgements.sql`, a separate release after
v0.5.0/schema 088. Historical delivery rows are not backfilled with inferred
receiver proof. The candidate still needs exact CI and managed release gates.
The current Stage deployment recipe pins v0.5.0 and requires an explicitly
reviewed version-resolution update before a v0.6.0 rollout.

## Delivery contract

Each delivery is a POST with `Content-Type: application/json` and these
headers:

* `X-Flare-Automation-Timestamp`: Unix seconds as a decimal string.
* `X-Flare-Automation-Signature`: `sha256=` followed by an HMAC-SHA256 digest.
* `X-Flare-Automation-Event`: one bounded event name such as `new_issue`,
  `regression`, `spike`, `service`, or `alert`. A channel test uses `test`;
  receivers must acknowledge it without starting remediation.

The signature covers the exact bytes delivered, using
`HMAC-SHA256(signing_secret, timestamp + "." + raw_body)`. A receiver must
compare the digest in constant time and reject timestamps outside a five-minute
replay window. It should also deduplicate an accepted timestamp/body or its
derived delivery key before starting downstream work. Queued sends preserve
the exact body across retries and use a fresh timestamp and signature for
each attempt. A receiver can use the SHA-256 of the verified raw body as its
durable deduplication key; the timestamp itself changes on retry.

The body is the fixed `flare.automation.v1` schema:

```json
{
  "schema": "flare.automation.v1",
  "event": "new_issue",
  "project_id": "project-id",
  "issue_id": "issue-id-or-empty",
  "event_count": 3,
  "observed_at": "2026-10-03T10:20:30.123Z"
}
```

It intentionally contains no issue title, culprit, stack trace, log text,
telemetry attributes, credentials, or model output. A watchdog event without a
grouped issue keeps `issue_id` as an empty string and remains routable by
`project_id`.

The channel test API requires an explicit project in the channel's routing
scope. It sends the same signed envelope with `event: "test"`, so operators can
verify delivery without creating an incident workflow.
Testing a paused channel is allowed. Check the response body's `ok` value;
HTTP 200 can also carry `ok:false`. This direct test does not create a durable
outbox delivery receipt. One real alert and its exact persisted
`receiver_enqueued` acknowledgement are required to prove that later boundary.

## Worker follow-up

After verifying the signature and replay window, a worker should authenticate
back to Flare with a tenant-scoped API key or session and fetch the current
issue/evidence. The `automation-next` projection exposes only the next eligible
human-gated stage and receipt metadata. Run `triage_issue` (or the REST triage
endpoint) against the current issue before preparing a patch. A fresh result
returns its 64-character `evidence_digest`; retain it as the patch receipt's
`evidence_ref`. Cached results omit the digest and cannot authorize a patch.
Flare checks that the digest belongs to a completed fresh
triage receipt for the same tenant, project, and issue before accepting a
passed patch. A worker may propose a remediation and wait for an approved
actor; Flare records stage receipts, while the worker and the deployment system
remain responsible for any separately authorized CI/deploy/synthetic actions.
Caller-reported passed CI, merge, deploy, and synthetic outcomes remain
`unverified` and cannot advance the next stage or accepted-deploy watchdog.
The project verification summary shows the pending 30-day backlog. A project
administrator can bind a revocable Stage profile, then a member can verify
each exact CI, merge, deploy, and synthetic receipt through Flare's Stage
verifier route. Flare persists Stage's matched proof before those receipts
advance the chain. Synthetic completion requires Stage's optional external
probe to run after cutover and retain a result bound to the same deploy run,
source commit, and image; an opaque worker result alone stays unverified.

Delivery health is visible through the channel's last-attempt/last-success
fields, alerts integration metrics, and aggregate `automation_outbox` state on
`/readyz`. For accepted issue events with enabled issue rules, ingest commits
the event and `issue_alert_evaluations` job together. The fenced evaluation
worker retries after a crash, and `/readyz.issue_evaluations` exposes pending,
expired, and dead work. The outbox retries non-2xx responses and timeouts with
the same body, up to its bounded attempt limit. A receiver must deduplicate
before starting work because a lost response can cause a successful notice to
be sent again. Dead evaluation or outbox rows require operator attention.
Ordinary email and Slack channels remain best effort; Flare's issue alert
rate limits may intentionally suppress excess notices.
