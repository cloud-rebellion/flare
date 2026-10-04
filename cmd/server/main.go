package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/gorilla/csrf"

	"github.com/bright-interaction/flare/internal/analytics"
	"github.com/bright-interaction/flare/internal/api"
	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/config"
	"github.com/bright-interaction/flare/internal/db"
	"github.com/bright-interaction/flare/internal/flarereport"
	"github.com/bright-interaction/flare/internal/partition"
)

//go:embed all:frontend/build
var frontendFiles embed.FS

// Version is stamped by the release build so Flare can group its own
// regressions by the exact deployed artifact. Local builds retain a readable
// fallback rather than reporting an empty release.
var Version = "dev"

func main() {
	flarereport.InitFlare("flare", Version)
	defer flarereport.Flush(2 * time.Second)
	if err := run(); err != nil {
		// Config, migration, and database failures happen before the HTTP server
		// can capture them. Report the terminal startup error while the DSN client
		// is still alive, then keep the existing stderr log for operators.
		flarereport.CaptureErr(err)
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// The chart passes its image tag as FLARE_RELEASE. Compare that declared
	// artifact with the version baked into the executable; do not let a healthy
	// pod advertise a release other than the code it is actually running.
	cfg.Release, err = releaseIdentity(Version, os.Getenv("FLARE_RELEASE"))
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if !cfg.IsProduction() {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	if err := db.RunMigrations(ctx, cfg.DatabaseURL); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	slog.Info("migrations applied")

	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBMinConns)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	slog.Info("database connected", "max_conns", cfg.DBMaxConns)

	// Analytics: embedded DuckDB attached read-only to Postgres + a Parquet cold
	// tier. Non-fatal: if it fails to open, the server still serves every read
	// via the hot tier; only the analytics endpoints + cold export are disabled.
	var analyticsMgr *analytics.Manager
	var exporter partition.Exporter
	if mgr, aerr := analytics.Open(ctx, analytics.Config{
		PostgresDSN: cfg.DatabaseURL,
		ParquetDir:  cfg.ParquetDir,
		S3Endpoint:  cfg.S3Endpoint, S3Bucket: cfg.S3Bucket,
		S3AccessKey: cfg.S3AccessKey, S3SecretKey: cfg.S3SecretKey,
		S3Region: cfg.S3Region, S3UseSSL: cfg.S3UseSSL,
	}); aerr != nil {
		slog.Warn("analytics disabled (DuckDB open failed)", "error", aerr)
	} else {
		analyticsMgr = mgr
		exporter = analytics.NewExporter(mgr, pool)
		defer mgr.Close()
	}

	// Partition manager: pre-create daily telemetry partitions, export aged ones
	// to the Parquet cold tier, then drop (retention = export-then-DROP).
	// FLARE_PARQUET_DIR always has a default, so a non-empty value proves
	// nothing about intent. Treat the cold tier as required unless the operator
	// explicitly opts out, and refuse to drop unarchived partitions otherwise.
	requireExport := !cfg.AllowDropWithoutExport
	go partition.New(pool, cfg.RetentionDays, exporter, requireExport).Run(ctx)
	slog.Info("partition manager started", "retention_days", cfg.RetentionDays,
		"cold_tier_available", exporter != nil, "require_export_before_drop", requireExport)
	if requireExport && exporter == nil {
		slog.Error("cold tier unavailable and FLARE_ALLOW_DROP_WITHOUT_EXPORT is not set: retention is PAUSED, telemetry partitions will accumulate. Fix analytics/DuckDB, or set FLARE_ALLOW_DROP_WITHOUT_EXPORT=true to prune without archiving")
	}

	sessions := auth.NewSessionManager(cfg.SessionLifetime, cfg.SessionIdleTimeout, cfg.IsProduction())
	sessions.Store = pgxstore.New(pool)

	var csrfMW func(http.Handler) http.Handler
	if cfg.DisableCSRF && !cfg.IsProduction() {
		slog.Warn("CSRF protection DISABLED via DISABLE_CSRF (dev only)")
		csrfMW = func(next http.Handler) http.Handler { return next }
	} else {
		// gorilla/csrf enforces an Origin/Referer check on state-changing
		// requests and validates against trusted origins. Trust the BASE_URL
		// host so the same-origin SPA passes.
		//
		// NOTE (GO-2025-3884): gorilla/csrf has an unfixed advisory about
		// TrustedOrigins validation. It is not reachable here: TrustedOrigins is
		// a single static host derived from BASE_URL (never attacker-supplied),
		// and the session cookie is SameSite=Strict, which structurally blocks
		// cross-site cookie-bearing POSTs on its own. Tracked for a future move
		// to a maintained CSRF implementation.
		host := cfg.BaseURL
		if u, err := url.Parse(cfg.BaseURL); err == nil && u.Host != "" {
			host = u.Host
		}
		csrfMW = csrf.Protect(
			[]byte(cfg.CSRFKey),
			csrf.Secure(cfg.IsProduction()),
			csrf.Path("/"),
			csrf.SameSite(csrf.SameSiteStrictMode),
			csrf.HttpOnly(true),
			csrf.TrustedOrigins([]string{host}),
		)
	}

	build, err := fs.Sub(frontendFiles, "frontend/build")
	if err != nil {
		return fmt.Errorf("frontend fs: %w", err)
	}

	srv := api.NewServer(pool, sessions, cfg, analyticsMgr)
	// Each replica may drain the same durable automation queue. Short fenced
	// leases preserve one stored notice across restarts; retries sign the same
	// body so receivers can deduplicate its verified hash.
	go srv.RunAutomationOutbox(ctx)

	// Watchdog: periodic anomaly + silence detection (the "abnormal behavior"
	// alerts that cannot ride on ingest). Best-effort; shares alert channels.
	go srv.RunWatchdog(ctx, 5*time.Minute)
	if cfg.ConnectorSyncEnabled {
		// Provider polling is an explicit operator choice. The scheduler is
		// bounded, due-gated, and lease-protected; leaving the flag off keeps
		// connector credentials entirely caller-driven.
		go srv.RunConnectorSyncScheduler(ctx, cfg.ConnectorSyncInterval)
	}

	// Re-surface outstanding right-to-erasure obligations on a ticker, not only at
	// the boot log below. A long-uptime instance would otherwise announce them
	// once and then go silent for weeks while the object store still holds the
	// data, and for a deleted org there is no tenant left to notice.
	go srv.RunErasureReminder(ctx, 6*time.Hour)

	handler := srv.Routes(build, csrfMW)

	httpServer := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: handler,
		// ReadHeaderTimeout alone only bounds the headers. Without ReadTimeout a
		// client can dribble a body (ingest accepts up to 1 MiB) for as long as
		// it likes, holding a goroutine and a connection each time; without
		// IdleTimeout, keep-alive connections are never reaped. WriteTimeout is
		// the widest of the three because a large trace/log read is legitimately
		// slow.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown", "error", err)
		}
		// Then drain detached work (alert dispatch, AI triage). Shutdown only
		// waits for in-flight HTTP requests; ingest hands alert evaluation to a
		// background worker and returns, so without this a SIGTERM during a
		// deploy silently dropped pages that were already being sent.
		srv.WaitBackground(shutdownCtx)
	}()

	// Outstanding right-to-erasure obligations, surfaced on every start.
	//
	// When PurgeColdScope refuses (S3 cold tier has no object rewrite), the hot
	// tier is erased and aged telemetry stays in the bucket. For an ORG deletion
	// there is no tenant left to chase it: the org row is gone, so there is no
	// dashboard to show it on and nobody to ask. The self-host operator is the
	// only person who can act, and a log line at the moment of deletion has
	// almost certainly rotated by the time anyone looks.
	//
	// Repeating it at startup means the obligation keeps announcing itself until
	// someone clears the row.
	srv.LogOpenErasures(context.Background())

	slog.Info("flare listening", "addr", httpServer.Addr, "env", cfg.Environment)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

func releaseIdentity(compiled, declared string) (string, error) {
	compiled = strings.TrimSpace(compiled)
	declared = strings.TrimSpace(declared)
	if compiled == "" {
		compiled = "dev"
	}
	// Local and Compose development builds have no immutable release. A Helm
	// release has a concrete tag and must use an image built with that tag.
	if declared == "" {
		return compiled, nil
	}
	if declared == "development" && (compiled == "dev" || compiled == "development") {
		return compiled, nil
	}
	if compiled != declared {
		return "", fmt.Errorf("FLARE_RELEASE %q does not match binary release %q", declared, compiled)
	}
	return compiled, nil
}
