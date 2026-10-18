module github.com/bright-interaction/flare

go 1.26.0

// Pinned to the toolchain the container image already builds with. Without this,
// go.mod permits an older Go and govulncheck reports the standard-library
// vulnerabilities fixed through 1.26.9 as reachable. The pin makes the
// version that keeps this safe a guarantee rather than a property of the base image.
toolchain go1.26.9

require (
	github.com/alexedwards/scs/pgxstore v0.0.0-20251002162104-209de6e426de
	github.com/alexedwards/scs/v2 v2.9.0
	github.com/getsentry/sentry-go v0.47.0
	github.com/go-chi/chi/v5 v5.3.0
	github.com/go-sourcemap/sourcemap v2.1.4+incompatible
	github.com/gorilla/csrf v1.7.3
	github.com/jackc/pgx/v5 v5.9.2
	github.com/marcboeker/go-duckdb/v2 v2.4.3
	github.com/nrednav/cuid2 v1.1.0
	github.com/pressly/goose/v3 v3.24.1
	go.opentelemetry.io/proto/otlp v1.10.0
	golang.org/x/crypto v0.57.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/apache/arrow-go/v18 v18.4.1 // indirect
	github.com/duckdb/duckdb-go-bindings v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/darwin-amd64 v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/darwin-arm64 v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/linux-amd64 v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/linux-arm64 v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/windows-amd64 v0.1.21 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/goccy/go-json v0.10.5 // indirect
	github.com/google/flatbuffers v25.2.10+incompatible // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/securecookie v1.1.2 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.18.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/marcboeker/go-duckdb/arrowmapping v0.0.21 // indirect
	github.com/marcboeker/go-duckdb/mapping v0.0.21 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/pierrec/lz4/v4 v4.1.22 // indirect
	github.com/sethvargo/go-retry v0.3.0 // indirect
	github.com/zeebo/xxh3 v1.0.2 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/exp v0.0.0-20250408133849-7e4ce0ab07d0 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/telemetry v0.0.0-20260811182544-a038080d80e5 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	golang.org/x/xerrors v0.0.0-20240903120638-7835f813f4da // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.83.1 // indirect
)
