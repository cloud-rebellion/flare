# Flare

Sovereign observability: errors, logs, and traces on one Postgres you control.

Self-hostable single binary (Go + embedded SvelteKit). No ClickHouse, no heavy
daemon. Storage scales up a ladder: time-partitioned Postgres (hot) -> embedded
DuckDB columnar queries -> Parquet on object storage (cold), all behind one
pluggable `Store` interface.

## What it does

- **Ingest**: Sentry-envelope and OTLP over HTTP. Point any Sentry or
  OpenTelemetry SDK at a project DSN.
- **Errors**: grouped issues with stack traces, source-map de-minification, and
  first/last-seen + frequency.
- **Logs and traces**: structured logs and spans on the same partitioned store,
  queried through DuckDB.
- **Alerts**: new-issue and threshold alerts routed to email and webhooks.
- **AI triage** (BYOAI, optional): LLM triage of new issues, bring your own key.
- **Analytics**: DuckDB rollups over the telemetry tables.
- **Multi-project**: orgs, projects, per-project DSNs and API keys, tenant-scoped
  throughout and enforced by a source-parsing CI guard.
- **MCP**: an agent API so an assistant can read and triage issues.

For endpoint setup, health semantics, provider-specific signals, and known
monitoring limits, see [docs/INTEGRATIONS.md](docs/INTEGRATIONS.md).
For product rollout order and live acceptance evidence, see
[docs/ACCEPTANCE_PLAN.md](docs/ACCEPTANCE_PLAN.md).

Copyable integration starters live under [`examples/flutter`](examples/flutter/)
and [`examples/backend`](examples/backend/). They cover Flutter runtime,
framework, async, native, and handled errors, backend Sentry or OTLP/HTTP
export, request correlation, scrubbing, and scheduled-job check-ins. They do
not modify another application's dependencies or deploy telemetry for it.

The hosted, multi-tenant control plane (auto-wiring DSNs across a fleet) is the
commercial overlay and is not in this repo. See [LICENSING.md](LICENSING.md).

## Architecture

- `cmd/server` - entrypoint; embeds the SvelteKit build via `go:embed`.
- `internal/config` - env config; production refuses to boot without secrets.
- `internal/db` - pgx pool, goose migrations (incl. partitioned telemetry
  tables), sqlc-generated queries.
- `internal/auth` - sessions (scs + pgxstore), bcrypt passwords, API keys.
- `internal/api` - chi router, handlers, CSRF, SPA fallback.
- `internal/ingest` - Sentry-envelope + OTLP ingest.
- `internal/ciguard` - source-parsing CI guard: every query reading a
  tenant-scoped table must filter on `org_id` (or carry an explicit
  `-- ciguard:allow-unscoped` marker).
- `frontend` - SvelteKit (Svelte 5 + Tailwind 4), built with Bun.

## Local development

```bash
# 1. Postgres
docker run -d --name flare-pg -e POSTGRES_PASSWORD=flare -e POSTGRES_DB=flare \
  -p 55432:5432 postgres:16-alpine

# 2. Backend (applies migrations on boot)
DATABASE_URL='postgres://postgres:flare@localhost:55432/flare?sslmode=disable' \
  PORT=8095 DISABLE_CSRF=true ENVIRONMENT=development \
  BASE_URL='http://localhost:8095' go run ./cmd/server

# 3. Frontend (hot reload, proxies /api to :8095)
cd frontend && bun install && bun run dev
```

`DISABLE_CSRF=true` is honored only when `ENVIRONMENT != production`.

## Build

```bash
cd frontend && bun install && bun run build
cp -r frontend/build cmd/server/frontend/build   # what go:embed serves
go build ./cmd/server
# or the whole container:
docker build -t flare .
```

## Deploy

Build the container (`docker build -t flare .`) and run it behind your reverse
proxy, or use the compose file and Helm chart under `deploy/` (see
[deploy/README.md](deploy/README.md)). Flare generates its `.env` secrets on
first run and preserves them across restarts. Set `BASE_URL`, `DATABASE_URL`,
and the SMTP vars for your environment; everything else has sane defaults.

## License

Fair-code under the Flare Sustainable Use License (see [LICENSE](LICENSE)): free
to self-host and use, but you can't resell it as a hosted service. The details
and the enterprise boundary are in [LICENSING.md](LICENSING.md).
