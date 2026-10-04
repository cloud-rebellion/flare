# Self-hosting Flare

Flare is a sovereign, self-hostable observability backend (errors + logs +
traces). It runs on one Postgres and grows with you - no ClickHouse, no Kafka,
no external services required.

Two ways to run it: Docker Compose (single box) or Helm (Kubernetes).

## Docker Compose

```bash
cd deploy
cp .env.example .env
# Fill in the secrets in .env:
#   SESSION_KEY        openssl rand -base64 32
#   CSRF_KEY           openssl rand -hex 16      (exactly 32 chars)
#   FLARE_SECRET_KEY   openssl rand -base64 32   (required; keep it forever)
#   FLARE_DB_PASSWORD  any strong string
#   FLARE_BASE_URL     the URL Flare is reached at
docker compose up -d --build
```

For a traceable release, build from a clean, exact checkout and set
`FLARE_COMMIT` to its full lowercase Git object ID before running Compose.
Keep `FLARE_RELEASE` as the version (for example `v0.4.0`), not a second
commit string. Compose passes both values into the browser and server builds
and checks the running binary against the declared values. `/readyz` then
reports `release: v0.4.0+g<commit>` and `source_commit: <commit>`. An empty
`source_commit` means the build was not attributed to a Git revision. Record
the built image ID or registry digest separately; the process cannot
independently prove which image digest the container runtime started.

Open `FLARE_BASE_URL`. The first account you register becomes the workspace
**owner**. Create a project to get a DSN, then point any `@sentry/*` SDK (just
change the DSN) or an OTLP exporter at it.

Put a TLS terminator (Caddy, nginx, Traefik) in front for production and set
`FLARE_BASE_URL` to the public `https://` URL.

## Kubernetes (Helm)

`flare/VERSION` is `v0.4.0`. Keep the chart's `image.tag` at that version
when upgrading. A tag alone can be moved in the registry. For an exact image,
set `image.digest=sha256:<64 lowercase hex digits>`; the Deployment then
pulls `repository@digest`. Set `image.sourceCommit` to the full commit
stamped into that image as `FLARE_COMMIT`. Flare rejects a runtime declaration
that differs from the binary. The chart cannot verify that the supplied digest
was built from the supplied commit; the build pipeline must record and verify
that binding.

```bash
helm install flare ./deploy/helm/flare \
  --set baseURL=https://flare.example.com \
  --set secrets.sessionKey=$(openssl rand -base64 32) \
  --set secrets.csrfKey=$(openssl rand -hex 16) \
  --set secrets.secretKey=$(openssl rand -base64 32) \
  --set postgres.password=$(openssl rand -hex 16) \
  --set ingress.enabled=true \
  --set ingress.host=flare.example.com \
  --set ingress.className=nginx
```

For a pinned rollout, add `--set image.digest=sha256:<digest>` and
`--set image.sourceCommit=<full-git-sha>` to the Helm command. Verify the
rendered image digest, the live pod image ID, and `/readyz` on the exact
deployed revision before accepting the release.

The chart bundles a single Postgres (`postgres.bundled=true`) with its own PVC.
To use an existing database instead:

```bash
helm install flare ./deploy/helm/flare \
  --set postgres.bundled=false \
  --set postgres.externalUrl='postgres://user:pass@host:5432/flare?sslmode=require' \
  ... (baseURL + secrets as above)
```

`secrets.secretKey` is FLARE_SECRET_KEY, which encrypts stored credentials
(BYOAI keys, OIDC client secrets, SMTP passwords) at rest. The pod refuses to
start without it, so leaving it out of the command above does not silently
disable encryption, it just fails.

You can also supply all secrets via a pre-created `Secret` (keys `session-key`,
`csrf-key`, `secret-key`, `database-url`, optional `smtp-password`) with
`--set secrets.existingSecret=my-secret`.

## Configuration

| Setting | Compose (`.env`) | Helm (`values.yaml`) | Notes |
|---|---|---|---|
| Public URL | `FLARE_BASE_URL` | `baseURL` | Used in DSNs + email links; must match how Flare is reached |
| Session secret | `SESSION_KEY` | `secrets.sessionKey` | 32+ bytes, required |
| CSRF secret | `CSRF_KEY` | `secrets.csrfKey` | exactly 32 bytes, required |
| At-rest secret | `FLARE_SECRET_KEY` | `secrets.secretKey` | required; encrypts BYOAI keys, OIDC client secrets and channel configs. Production refuses to boot without it, and rotating it makes existing encrypted values unreadable |
| DB password | `FLARE_DB_PASSWORD` | `postgres.password` | for the bundled Postgres |
| External DB | `DATABASE_URL` | `postgres.externalUrl` (+ `bundled=false`) | point at your own Postgres |
| Retention (days) | `FLARE_RETENTION_DAYS` | `retentionDays` | before export-to-Parquet + drop |
| HubSpot scheduler | `FLARE_CONNECTOR_SYNC_ENABLED`, `FLARE_CONNECTOR_SYNC_INTERVAL` | n/a | optional; disabled by default, enables bounded due HubSpot account health passes (default interval `5m`) |
| Prune without archiving | `FLARE_ALLOW_DROP_WITHOUT_EXPORT` | `allowDropWithoutExport` | default off. Retention only drops a partition after it is archived, so an unreachable cold tier pauses retention (partitions accumulate, logged as an error every cycle) instead of deleting unarchived telemetry. Set to `true` only if you run with no cold tier on purpose |
| Email | `SMTP_*` | `smtp.*` | optional: alert emails + password reset |

### Email (optional)

Enables alert delivery and password reset. Works with any SMTP server. Set
`SMTP_HOST`, `SMTP_FROM`, credentials, and `SMTP_TLS` (`tls` for port 465,
`starttls` for 587, `none` for dev). Leave `SMTP_HOST` blank to run without
email - those features simply no-op.

### Single sign-on (optional)

Configure OIDC per workspace in **Settings -> Single sign-on** (admin). Register
an OIDC app in your IdP (Zitadel/Okta/Auth0/Keycloak/Entra) with the redirect
URI shown there, then paste the client id + secret.

## Backups

Back up the Postgres volume (`flare-data` / the DB PVC) and the Parquet volume
(`flare-app-data` / the data PVC). Postgres holds the hot tier; Parquet holds
aged telemetry.

## Upgrading

Pull the new image and `docker compose up -d` (or `helm upgrade`). Schema
migrations run automatically at startup.
