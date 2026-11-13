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

// Version and Commit are stamped by the image builder. A version alone can be
// reused across builds; only a supplied, pipeline-bound commit distinguishes
// one source revision from another. Local builds keep a readable fallback.
var Version = "dev"
var Commit = ""

func main() {
	flarereport.InitFlare("flare", exactRelease(Version, Commit))
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
	// Compose and Helm declare the version and source commit used at build
	// time. Reject a different runtime declaration before serving readiness.
	cfg.Release, err = releaseIdentity(Version, os.Getenv("FLARE_RELEASE"))
	if err != nil {
		return err
	}
	cfg.SourceCommit, err = sourceCommitIdentity(Commit, os.Getenv("FLARE_COMMIT"))
	if err != nil {
		return err
	}
	cfg.Release = exactRelease(cfg.Release, cfg.SourceCommit)

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

	// Warn loudly if no workspace can manage instance-wide settings.
	_, _ = srv.CheckOperatorOrg(ctx)

	// Partition manager: pre-create daily telemetry partitions, export aged ones
	// to the Parquet cold tier, then drop (retention = export-then-DROP), and
	// prune archive days past cold retention. The policy is read from the
	// dashboard setting on EVERY pass (RETENTION_DAYS / COLD_RETENTION_DAYS are
	// the fallback when nothing is stored), so a change needs no restart.
	// FLARE_PARQUET_DIR always has a default, so a non-empty value proves
	// nothing about intent. Treat the cold tier as required unless the operator
	// explicitly opts out, and refuse to drop unarchived partitions otherwise.
	requireExport := !cfg.AllowDropWithoutExport
	partitions := partition.New(pool, cfg.RetentionDays, exporter, requireExport).WithPolicy(srv.RetentionPolicy)
	if analyticsMgr != nil {
		partitions.WithColdPruner(analyticsMgr)
	}
	srv.SetRetentionStatusSource(partitions.LastStatus)
	go partitions.Run(ctx)
	slog.Info("partition manager started", "env_retention_days", cfg.RetentionDays,
		"env_cold_retention", string(cfg.ColdRetentionMode), "env_cold_retention_days", cfg.ColdRetentionDays,
		"cold_tier_available", exporter != nil, "require_export_before_drop", requireExport)
	if requireExport && exporter == nil {
		slog.Error("cold tier unavailable and FLARE_ALLOW_DROP_WITHOUT_EXPORT is not set: retention is PAUSED, telemetry partitions will accumulate. Fix analytics/DuckDB, set FLARE_ALLOW_DROP_WITHOUT_EXPORT=true, or choose \"do not archive\" in Settings to prune without archiving")
	}
	// Each replica may drain the same durable automation queue. Short fenced
	// leases preserve one stored notice across restarts; retries sign the same
	// body so receivers can deduplicate its verified hash.
	go srv.RunAutomationOutbox(ctx)
	// Failed check-ins commit their per-channel notices with monitor state.
	// Every replica may reclaim expired leases and retry routed destinations.
	go srv.RunMonitorFailureOutbox(ctx)
	// Event and alert evaluation now share a commit. Every replica may drain
	// the fenced queue; a saturated or restarting process cannot discard an
	// accepted event's issue-alert handoff.
	go srv.RunIssueAlertEvaluations(ctx)
	// Native metric retry keys expire after seven days. Each replica removes a
	// bounded batch of expired identities without touching retained receipts.
	go srv.RunNativeMetricAcceptancePrune(ctx)
	// Native log retry identities have the same bounded seven-day window.
	go srv.RunNativeLogAcceptancePrune(ctx)
	// Public connector reports have a separate bounded seven-day retry window.
	go srv.RunClientConnectorAcceptancePrune(ctx)
	// Custom OTLP/HTTP senders may opt into per-batch retry keys. Sweep their
	// expired identities without touching telemetry or acceptance receipts.
	go srv.RunOTLPAcceptancePrune(ctx)

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

func sourceCommitIdentity(compiled, declared string) (string, error) {
	compiled = strings.TrimSpace(compiled)
	declared = strings.TrimSpace(declared)
	if compiled != "" {
		if len(compiled) != 40 && len(compiled) != 64 {
			return "", fmt.Errorf("binary source commit is not a full Git object ID")
		}
		for _, c := range compiled {
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
				return "", fmt.Errorf("binary source commit is not a lowercase Git object ID")
			}
		}
	}
	if compiled != declared {
		return "", fmt.Errorf("FLARE_COMMIT does not match binary source commit")
	}
	return compiled, nil
}

func exactRelease(version, commit string) string {
	if commit == "" {
		return version
	}
	return version + "+g" + commit
}
