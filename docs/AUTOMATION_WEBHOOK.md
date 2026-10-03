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
  "config": {
    "url": "https://worker.example.com/flare",
    "signing_secret": "a-random-secret-at-least-16-bytes"
  }
}
```

The URL must be a public HTTPS endpoint. Loopback, private, link-local,
metadata, and other blocked destinations are rejected at creation and the
delivery client checks the resolved address again. The signing secret must be
16–256 printable bytes. Both values are encrypted at rest and redacted from
channel responses and delivery errors.

## Delivery contract

Each delivery is a POST with `Content-Type: application/json` and these
headers:

* `X-Flare-Automation-Timestamp`: Unix seconds as a decimal string.
* `X-Flare-Automation-Signature`: `sha256=` followed by an HMAC-SHA256 digest.
* `X-Flare-Automation-Event`: one bounded event name such as `new_issue`,
  `regression`, `spike`, `service`, or `alert`.

The signature covers the exact bytes delivered, using
`HMAC-SHA256(signing_secret, timestamp + "." + raw_body)`. A receiver must
compare the digest in constant time and reject timestamps outside a five-minute
replay window. It should also deduplicate an accepted timestamp/body or its
derived delivery key before starting downstream work.

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

Delivery health is visible through the channel's last-attempt/last-success
fields and the existing alerts integration metrics. Treat a non-2xx response or
timeout as a delivery failure; retry accepted signed notices idempotently on the
receiver, and do not blindly retry a notice rejected for an invalid or stale
signature.
