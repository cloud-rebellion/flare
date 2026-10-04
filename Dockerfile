# Flare observability engine (self-contained build).
#
# Local build:
#   cd flare && docker build -t flare .
#
# CGO note: the analytics package uses marcboeker/go-duckdb (DuckDB columnar
# reads + Parquet cold tier), which needs CGO + glibc. The backend stage builds
# on golang:1.26.4-bookworm (glibc) and the runtime is debian:bookworm-slim,
# matching atomicsite's analyticsdb build.

# Stage 1: SvelteKit dashboard.
FROM oven/bun:1-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package.json frontend/bun.lock ./
RUN --mount=type=cache,target=/root/.bun/install/cache \
    bun install --frozen-lockfile
COPY frontend/ .
# These values are deliberately limited to public dashboard metadata. The DSN
# is an ingest-only credential; never pass member/API keys or provider secrets
# as build arguments because Vite/SvelteKit embeds them in the browser bundle.
ARG FLARE_DSN=""
ARG FLARE_ENVIRONMENT=production
ARG FLARE_RELEASE=v0.4.0
ARG FLARE_COMMIT=""
ENV PUBLIC_FLARE_DSN=${FLARE_DSN} \
    PUBLIC_FLARE_ENVIRONMENT=${FLARE_ENVIRONMENT} \
    PUBLIC_FLARE_RELEASE=${FLARE_RELEASE}
RUN if [ -n "${FLARE_COMMIT}" ]; then \
      export PUBLIC_FLARE_RELEASE="${FLARE_RELEASE}+g${FLARE_COMMIT}"; \
    fi; \
    bun run build --sourcemap hidden && bun scripts/stage-source-maps.mjs
# CI may extract /app/frontend/source-maps from this named stage to upload maps
# for this exact bundle. They are outside /app/frontend/build, which is the
# only frontend directory copied into the served image below.

# Stage 2: Go server (CGO for go-duckdb). The frontend build is copied in for go:embed.
FROM golang:1.26.6-bookworm AS backend
WORKDIR /app
ARG FLARE_RELEASE=v0.4.0
ARG FLARE_COMMIT=""
RUN apt-get update -qq \
    && apt-get install -y -qq --no-install-recommends git ca-certificates build-essential gcc g++ \
    && rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
COPY --from=frontend /app/frontend/build ./cmd/server/frontend/build
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w -X main.Version=${FLARE_RELEASE} -X main.Commit=${FLARE_COMMIT}" -o /flare ./cmd/server

# Stage 3: runtime (debian-slim for glibc + libstdc++, which the duckdb static
# lib needs at runtime).
FROM debian:bookworm-slim
ARG FLARE_RELEASE=v0.4.0
ARG FLARE_COMMIT=""
LABEL org.opencontainers.image.version="${FLARE_RELEASE}" \
      org.opencontainers.image.revision="${FLARE_COMMIT}"
RUN apt-get update -qq \
    && apt-get install -y -qq --no-install-recommends \
       ca-certificates tzdata wget libstdc++6 libgcc-s1 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 1000 flare && useradd -u 1000 -g flare -m -d /app flare \
    && mkdir -p /app/data/parquet && chown -R flare:flare /app
COPY --from=backend /flare /usr/local/bin/flare
USER flare
ENV FLARE_PARQUET_DIR=/app/data/parquet
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/livez || exit 1
ENTRYPOINT ["/usr/local/bin/flare"]
