// Package analytics is a read-only embedded DuckDB layer for columnar
// aggregations over Flare's telemetry. It ATTACHes the live Postgres
// (read-only, via the postgres extension) as schema "hot" and unions hot
// partitions with cold Parquet (local dir or S3/MinIO), mirroring the
// atomicsite analyticsdb pattern.
//
// Writes never go through here; the hot tier (partitioned Postgres) still
// serves every dashboard read. This is additive columnar headroom. Open is
// non-fatal: if DuckDB fails to load, the server runs without analytics.
package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	_ "github.com/marcboeker/go-duckdb/v2"
)

// Config is everything the analytics layer needs. PostgresDSN is attached
// read-only; ParquetRoot is where the cold tier lives (a local dir, or an
// s3:// URL when S3Endpoint is set). Secrets are passed in, never logged.
type Config struct {
	PostgresDSN string
	ParquetDir  string // local cold-tier dir (used when S3Endpoint == "")

	S3Endpoint  string // optional; when set, cold tier is s3://S3Bucket/flare
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
	S3Region    string
	S3UseSSL    bool
}

type Manager struct {
	db  *sql.DB
	cfg Config

	mu     sync.RWMutex
	closed bool
}

// parquetRoot is the base URI the export worker writes to and queries read.
func (c Config) parquetRoot() string {
	if c.S3Endpoint != "" {
		return "s3://" + c.S3Bucket + "/flare"
	}
	return strings.TrimRight(c.ParquetDir, "/")
}

// Open creates the in-memory DuckDB, loads extensions, attaches Postgres
// read-only, and (optionally) registers the S3 secret for MinIO. Errors are
// returned so the caller can log + continue without analytics.
func Open(ctx context.Context, cfg Config) (*Manager, error) {
	if strings.TrimSpace(cfg.PostgresDSN) == "" {
		return nil, fmt.Errorf("analytics: PostgresDSN is required")
	}

	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("analytics: open duckdb: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)

	// postgres_scanner (loaded as "postgres") for the hot ATTACH; httpfs only
	// when an S3 endpoint is configured. parquet is bundled in DuckDB core.
	exts := []string{"postgres"}
	if cfg.S3Endpoint != "" {
		exts = append(exts, "httpfs")
	}
	for _, ext := range exts {
		if _, err := db.ExecContext(ctx, "INSTALL "+ext); err != nil {
			slog.Debug("analytics: install extension (treating as preloaded)", "ext", ext, "err", err)
		}
		if _, err := db.ExecContext(ctx, "LOAD "+ext); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("analytics: load %s: %w", ext, err)
		}
	}

	// READ_ONLY attach: analytics must never write to the ingest DB.
	attach := fmt.Sprintf("ATTACH '%s' AS hot (TYPE postgres, READ_ONLY)", escapeSQL(cfg.PostgresDSN))
	if _, err := db.ExecContext(ctx, attach); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("analytics: attach postgres: %w", err)
	}

	if cfg.S3Endpoint != "" {
		secret := fmt.Sprintf(
			"CREATE OR REPLACE SECRET flare_s3 (TYPE s3, KEY_ID '%s', SECRET '%s', ENDPOINT '%s', URL_STYLE 'path', USE_SSL %t, REGION '%s')",
			escapeSQL(cfg.S3AccessKey), escapeSQL(cfg.S3SecretKey), escapeSQL(cfg.S3Endpoint), cfg.S3UseSSL, escapeSQL(cfg.S3Region),
		)
		if _, err := db.ExecContext(ctx, secret); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("analytics: create s3 secret: %w", err)
		}
	}

	slog.Info("analytics: ready", "cold_tier", cfg.parquetRoot(), "s3", cfg.S3Endpoint != "")
	return &Manager{db: db, cfg: cfg}, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	if _, err := m.db.Exec("DETACH hot"); err != nil {
		slog.Debug("analytics: detach hot", "err", err)
	}
	return m.db.Close()
}

// Healthy returns true when DuckDB can read the attached Postgres.
func (m *Manager) Healthy(ctx context.Context) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return false
	}
	row := m.db.QueryRowContext(ctx, "SELECT 1 FROM hot.public.projects LIMIT 1")
	var n int
	if err := row.Scan(&n); err != nil && err != sql.ErrNoRows {
		slog.Warn("analytics: healthcheck failed", "err", err)
		return false
	}
	return true
}

// ErrClosed is returned by cold-tier work that could not run because the
// manager is shut down. It must be an error and never a nil "nothing to do":
// partition retention drops a partition once the export reports success, so a
// silent skip here is permanent telemetry loss.
var ErrClosed = errors.New("analytics: manager is closed")

// holdColdTier runs fn while holding the SHARED analytics lock.
//
// It exists so writers to the Parquet cold tier (the partition exporter) are
// mutually exclusive with the eraser (PurgeColdScope, which takes the exclusive
// lock). Without it the export ran on e.duck.db with no lock at all, and a
// right-to-erasure request could interleave like this:
//
//	exporter reads the hot partition, project P still present
//	handler commits DELETE project P
//	handler purges the cold tier: dt=D holds only part.parquet.tmp, which the
//	  purge's glob does not match, so it finds nothing to erase
//	exporter renames part.parquet.tmp -> part.parquet
//
// and P's telemetry is back in the cold tier after Flare answered 204 to an
// erasure request. Holding the shared lock across the whole export closes that
// window: the purge waits for the rename, then erases the file it produced.
//
// Shared and not exclusive on purpose. An export COPY of a full day is slow,
// and blocking every analytics read for its duration would be a worse bug than
// the one being fixed. Exports of different days write different files, so they
// do not conflict with each other, only with the eraser.
func (m *Manager) holdColdTier(fn func() error) error {
	if m == nil {
		return ErrClosed
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return ErrClosed
	}
	return fn()
}

// escapeSQL escapes single quotes for embedding in a DuckDB string literal.
func escapeSQL(s string) string { return strings.ReplaceAll(s, "'", "''") }

// coldTables are the tables whose aged partitions are exported to Parquet.
var coldTables = []string{"events", "logs", "spans", "metrics"}

// PurgeColdScope physically erases every row matching column=value from the
// Parquet cold tier, so a project/org deletion (right-to-erasure) also reaches
// telemetry that has already aged out of the hot Postgres tier. Each affected
// Parquet file is rewritten without the scope's rows; a file that becomes empty
// is removed. column must be "project_id" or "org_id"; value is escaped. No-op
// when DuckDB is unavailable or the local cold tier is empty. It takes the write
// lock (rare, delete-time) so a concurrent analytics read cannot see a
// half-rewritten file. S3 cold tiers are not purged here (no object delete) and
// return an error so the caller can surface it rather than silently skip.
func (m *Manager) PurgeColdScope(ctx context.Context, column, value string) error {
	if m == nil {
		return nil
	}
	switch column {
	case "project_id", "org_id":
	default:
		return fmt.Errorf("analytics: invalid purge column %q", column)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	if m.cfg.S3Endpoint != "" {
		return fmt.Errorf("analytics: cold-tier purge not supported on S3 (erase %s=%s in the bucket manually)", column, value)
	}
	esc := escapeSQL(value)
	// Through the shared archive walker, like pruning: the day is opened and
	// verified fd-relative, and every delete and rename is relative to it, so
	// a dt= symlink cannot redirect the erasure outside the archive. A day
	// that cannot be opened safely makes the purge fail, never skip quietly.
	var unsafe []error
	err := walkArchiveDays(m.cfg.ParquetDir, coldTables, nil, func(d *archiveDay) error {
		// Crash residue first. Under the exclusive lock no export or purge is
		// in flight, so a .tmp or .purge here is left over from one that died,
		// and it holds rows too: an erasure that answered 204 with them on
		// disk was incomplete. Nothing reads them, so they are deleted, not
		// rewritten.
		for _, name := range []string{"part.parquet.tmp", "part.parquet.purge"} {
			if d.has(name) {
				if err := d.remove(name); err != nil {
					return fmt.Errorf("analytics: purge %s/%s/%s: %w", d.Table, d.Name, name, err)
				}
			}
		}
		if d.has("part.parquet") {
			if err := m.purgeFile(ctx, d, column, esc); err != nil {
				return fmt.Errorf("analytics: purge %s/%s: %w", d.Table, d.Name, err)
			}
		}
		_ = d.removeSelf() // only succeeds once the day is empty
		return nil
	}, func(table, name string, err error) {
		unsafe = append(unsafe, fmt.Errorf("analytics: purge %s/%s: %w", table, name, err))
	})
	if err != nil {
		return err
	}
	return errors.Join(unsafe...)
}

// purgeFile rewrites one day's part.parquet without rows where
// column='escVal'. DuckDB reads and writes by path (d.Path, verified when the
// day was opened); the remove and the replace are relative to the day.
// Assumes the caller holds the write lock.
func (m *Manager) purgeFile(ctx context.Context, d *archiveDay, column, escVal string) error {
	file := filepath.ToSlash(filepath.Join(d.Path, "part.parquet"))
	// archivedDay reads the file without the dt column (hive or physical), so
	// the rewrite never adds it and drops it from files an older purge wrote.
	src, err := m.archivedDay(ctx, file)
	if err != nil {
		return err
	}
	var total, remaining int64
	if err := m.db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s", src)).Scan(&total); err != nil {
		return err
	}
	if err := m.db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE %s <> '%s'", src, column, escVal)).Scan(&remaining); err != nil {
		return err
	}
	if remaining == total {
		return nil // scope not present in this file
	}
	if remaining == 0 {
		return d.remove("part.parquet")
	}
	copySQL := fmt.Sprintf("COPY (SELECT * FROM %s WHERE %s <> '%s') TO '%s' (FORMAT parquet, COMPRESSION zstd)",
		src, column, escVal, escapeSQL(file+".purge"))
	if _, err := m.db.ExecContext(ctx, copySQL); err != nil {
		return err
	}
	if err := d.rename("part.parquet.purge", "part.parquet"); err != nil { // atomic replace
		_ = d.remove("part.parquet.purge")
		return err
	}
	return nil
}
