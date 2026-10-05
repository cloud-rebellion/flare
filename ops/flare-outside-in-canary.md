# Independent Flare outside-in canary candidate

This is a **disabled-by-default deployment candidate**. It is not installed or
enabled by this change. Run it from a host outside the Flare and Stage production
host and failure domain. A Stage timer on the shared host cannot detect that
host's loss.

Each bounded cycle calls the fixed public origin
`https://flare.cloudrebellion.tech`. It checks `/readyz` status and release
identity against the exact expected commit and release in private config,
sends one fresh log to a dedicated canary project using an
idempotency key, reads back the terminal receipt from that project, then
searches that same project for the exact log.
The query has a fixed `flare-canary-` search term, a one-hour window, and a
five-row limit. Only the returned body is matched against the unique marker,
so that marker is not exposed in public access-log URLs. Ingest transport or 503
ambiguity gets one retry with identical payload and key. Receipt visibility
gets three attempts; log query visibility gets five. A failure sends a direct
SMTP alert through Resend. Alert state
is persisted so identical failures are repeated at most every six hours;
recovery sends a separate message. The script logs only fixed status codes,
never a key, marker, response body, or URL containing a marker.
An optional external deadman success URL is called only after the same log is
accepted, its terminal receipt is read back, and the project query finds it.
A failed success ping fails the cycle and uses the independent SMTP failure
alert. No ping is sent on a failed Flare probe. The URL stays in the private
config and is never printed or included in the alert.

## Required provisioning before any enablement

1. Confirm the native log atomic receipt release is deployed. Record its exact
   `source_commit` and `release` from an authorized Stage deployment receipt
   and verify they match the public `/readyz` response. Update the private
   config for each authorized deployment before enabling the next cycle;
   an unexpected identity causes a `release_identity` failure.
   The script
   expects HTTP 200 with `accepted: 1`, `rejected: 0`, `dropped: 0`, and a
   nonempty `receipt_id` in JSON. The native route does not currently send
   `X-Flare-Receipt-ID`; if a future release adds it, the script checks it
   against the JSON field. A weaker response fails.
2. Create a dedicated Flare canary organization and project, its DSN public
   key, and a separate read-only viewer API key for that organization. Flare
   viewer keys are organization-scoped, so reusing a production organization
   would let this host read other projects. The DSN numeric ID and project ID
   must refer to the **same project**.
3. Provision a Resend SMTP API key and verified sender domain for an operator
   mailbox outside Flare. Resend's SMTP endpoint is `smtp.resend.com:587` with
   username `resend`. Check the target inbox during the live route test.
4. On the independent Linux host, install Python 3 and create an unprivileged
   `flare-canary` account. Install the script at
   `/usr/local/libexec/flare_outside_in_canary.py` and the two systemd units at
   `/etc/systemd/system/`. Provision `/etc/flare-outside-in-canary.json` as a
   regular file readable only by that account, mode `0600`. Do not put keys in
   systemd arguments, environment, journald, or the repository.

Config shape (replace every placeholder on the independent host):

```json
{
  "dsn_id": "12345",
  "public_key": "CANARY_DSN_PUBLIC_KEY",
  "project_id": "CANARY_PROJECT_ID",
  "viewer_api_key": "flr_CANARY_VIEWER_KEY",
  "expected_source_commit": "0000000000000000000000000000000000000000",
  "expected_release": "FLARE_RELEASE_ID",
  "resend_api_key": "re_CANARY_RESEND_KEY",
  "from_email": "alerts@example.com",
  "to_email": "operator@example.com",
  "deadman_url": null
}
```

The target origin, ingress marker, and SMTP server are fixed in source. The
script rejects redirects and oversized responses, ignores proxy environment variables,
non-JSON responses, and a missing expected ingress header. TLS uses the system
trust store. The file parser rejects group/other-readable config. Logs are
queried with the read-only key; the public DSN key is used only for ingest.
To use a separate deadman service, replace `null` with its private HTTPS
success-ping URL and set the service's expected cadence to the 15-minute timer
plus enough grace for a delayed or failed run. The script accepts only an HTTPS
URL without userinfo or a fragment, never follows redirects, ignores proxy
environment variables, and uses a five-second request timeout. HTTP 2xx is
required; a network, TLS, redirect, or non-2xx result is `deadman_unavailable`.
Keep the endpoint outside the Flare/Stage host and failure domain, and verify a
missed ping reaches the operator independently of the canary's SMTP route.
Omitting `deadman_url` or setting it to `null` leaves this feature disabled.

## Live promotion gate

On the independent host, after credential provisioning and an explicit
external-host deployment review:

```sh
sudo -u flare-canary python3 /usr/local/libexec/flare_outside_in_canary.py --send-test-alert
sudo systemctl daemon-reload
sudo systemctl start flare-outside-in-canary.service
sudo systemctl status flare-outside-in-canary.service
sudo systemctl enable --now flare-outside-in-canary.timer
```

The first command sends a real test email: verify arrival in the operator
mailbox, not only SMTP acceptance. The one-shot run must show `CANARY_OK`, and
the dedicated project must show the test log. Only then enable the timer.
Provision and test the separate deadman alert before relying on the timer so
loss of the canary host or its alert credentials is visible. Review each
15-minute run and its journald status. To disable, run
`systemctl disable --now flare-outside-in-canary.timer` on that independent
host.

This proves the public readiness route, accepted native log receipt, and
read-back through a viewer query at that moment. It does not prove browser
execution, all telemetry pillars, inbox delivery, or availability of the
independent host. The alert route itself needs a live inbox test.

Offline tests:

```sh
python3 -m unittest discover -s flare/ops -p test_flare_outside_in_canary.py -v
```
