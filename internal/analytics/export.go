package analytics

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Exporter archives aged telemetry partitions to the Parquet cold tier. It uses
// the DuckDB manager (read-only Postgres attach) to COPY straight from the live
// partition, and the pgx pool to write the export_log ledger (DuckDB's attach
// is read-only, so the ledger write cannot go through it).
type Exporter struct {
	duck *Manager
	pool *pgxpool.Pool
}

func NewExporter(duck *Manager, pool *pgxpool.Pool) *Exporter {
	return &Exporter{duck: duck, pool: pool}
}

// ExportDay exports the dated child partition (e.g. logs_2026_05_20) of table to
// Parquet, verifies the written row count matches the source, and records it in
// export_log. Idempotent: a (table, day) already logged is skipped. Returns nil
// when there is nothing to export (partition missing or empty). The caller
// (partition retention) must only DROP after this returns nil.
// The whole body runs under holdColdTier (the SHARED analytics lock), so an
// export can never interleave with PurgeColdScope, which erases the cold tier
// for a right-to-erasure request under the exclusive lock. See holdColdTier for
// the interleaving that used to resurrect erased telemetry.
func (e *Exporter) ExportDay(ctx context.Context, table string, day time.Time) error {
	return e.duck.holdColdTier(func() error { return e.exportDay(ctx, table, day) })
}

func (e *Exporter) exportDay(ctx context.Context, table string, day time.Time) error {
	day = day.UTC()
	dayStr := day.Format("2006-01-02")
	child := fmt.Sprintf("%s_%s", table, day.Format("2006_01_02"))
	started := time.Now()

	var existingStatus string
	if err := e.pool.QueryRow(ctx, `SELECT status FROM export_log WHERE tbl=$1 AND day=$2`, table, day).Scan(&existingStatus); err != nil && err != pgx.ErrNoRows {
		return fmt.Errorf("read export_log: %w", err)
	}
	if existingStatus == "succeeded" {
		return nil // already exported
	}
	if err := e.beginAttempt(ctx, table, day); err != nil {
		return fmt.Errorf("begin export attempt: %w", err)
	}
	fail := func(code string, err error) error {
		e.recordFailure(ctx, table, day, started, code)
		return fmt.Errorf("%s: %w", code, err)
	}

	// Only a genuinely ABSENT partition is "nothing to archive". Deciding that
	// from a failed DuckDB count conflated every transient DuckDB fault (attach
	// dropped, extension missing, disk full, read-only replica lag) with
	// "missing", and the caller drops the partition when this returns nil - so a
	// blip meant permanent, silent telemetry loss. Ask Postgres directly, over
	// the pgx pool, and only then return nil.
	var childExists bool
	if err := e.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relkind = 'r')`, child,
	).Scan(&childExists); err != nil {
		return fail("partition_probe", fmt.Errorf("check partition %s exists: %w", child, err))
	}
	if !childExists {
		if err := e.recordSuccess(ctx, table, day, 0, 0, true, started); err != nil {
			return fmt.Errorf("record absent partition: %w", err)
		}
		return nil // child partition does not exist; nothing to archive
	}

	d := e.duck.db
	var srcCount int64
	if err := d.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM hot.public.%s", child)).Scan(&srcCount); err != nil {
		return fail("source_read", fmt.Errorf("count rows in %s: %w", child, err))
	}
	if srcCount == 0 {
		if err := e.recordSuccess(ctx, table, day, 0, 0, true, started); err != nil {
			return fmt.Errorf("record empty export: %w", err)
		}
		return nil
	}

	root := e.duck.cfg.parquetRoot()
	destDir := root + "/" + table + "/dt=" + dayStr
	final := destDir + "/part.parquet"
	writeTarget := final
	local := e.duck.cfg.S3Endpoint == ""
	if local {
		if err := os.MkdirAll(filepath.FromSlash(destDir), 0o755); err != nil {
			return fail("cold_write", fmt.Errorf("mkdir parquet dir: %w", err))
		}
		writeTarget = final + ".tmp"
	}

	copySQL := fmt.Sprintf("COPY (SELECT * FROM hot.public.%s) TO '%s' (FORMAT parquet, COMPRESSION zstd)", child, escapeSQL(writeTarget))
	if _, err := d.ExecContext(ctx, copySQL); err != nil {
		return fail("cold_write", fmt.Errorf("copy %s to parquet: %w", child, err))
	}

	var pqCount int64
	if err := d.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM read_parquet('%s')", escapeSQL(writeTarget))).Scan(&pqCount); err != nil {
		return fail("verify", fmt.Errorf("verify parquet: %w", err))
	}
	if pqCount != srcCount {
		return fail("verify", fmt.Errorf("parquet row count %d != source %d for %s (not committing)", pqCount, srcCount, child))
	}

	if local {
		if err := os.Rename(filepath.FromSlash(writeTarget), filepath.FromSlash(final)); err != nil {
			return fail("cold_write", fmt.Errorf("rename parquet: %w", err))
		}
	}

	var bytes int64
	bytesKnown := false
	if local {
		info, err := os.Stat(filepath.FromSlash(final))
		if err != nil {
			return fail("cold_stat", fmt.Errorf("stat parquet: %w", err))
		}
		bytes = info.Size()
		bytesKnown = true
	}
	if err := e.recordSuccess(ctx, table, day, srcCount, bytes, bytesKnown, started); err != nil {
		return fmt.Errorf("write export_log: %w", err)
	}
	return nil
}

// beginAttempt writes a single bounded state row before touching the cold tier.
// The row is global operational metadata: it intentionally contains no tenant,
// provider, path, credential, or payload fields.
func (e *Exporter) beginAttempt(ctx context.Context, table string, day time.Time) error {
	_, err := e.pool.Exec(ctx, `
		INSERT INTO export_log (tbl, day, status, attempted_at, exported_bytes, bytes_known, export_latency_ms, error_code, row_count)
		VALUES ($1,$2,'attempted',now(),0,false,0,'',0)
		ON CONFLICT (tbl,day) DO UPDATE SET status='attempted', attempted_at=now(), exported_bytes=0,
		  bytes_known=false, export_latency_ms=0, error_code=''`, table, day)
	return err
}

// recordFailure stores only a fixed stage token. Raw provider/SQL errors stay
// in process logs and never become durable API-visible telemetry.
func (e *Exporter) recordFailure(ctx context.Context, table string, day, started time.Time, code string) {
	code = boundedExportErrorCode(code)
	latency := time.Since(started).Milliseconds()
	if latency < 0 {
		latency = 0
	}
	// A timeout in COPY/verification cancels the caller's context. Use a short
	// detached write context so the durable failure is still visible for the
	// next retention cycle; the raw failure remains in the caller's log only.
	writeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = e.pool.Exec(writeCtx, `
		UPDATE export_log
		SET status='failed', attempted_at=now(), exported_bytes=0, bytes_known=false,
		    export_latency_ms=$3, error_code=$4
		WHERE tbl=$1 AND day=$2`, table, day, latency, code)
}

var exportErrorCodes = map[string]struct{}{
	"partition_probe": {},
	"source_read":     {},
	"cold_write":      {},
	"cold_stat":       {},
	"verify":          {},
}

// boundedExportErrorCode keeps durable exporter state to a small, documented
// vocabulary. Raw SQL/provider errors remain process logs only.
func boundedExportErrorCode(code string) string {
	if _, ok := exportErrorCodes[code]; ok {
		return code
	}
	return "unknown"
}

// recordSuccess finalizes the latest attempt. bytesKnown is true for local
// files (and empty partitions); DuckDB's current httpfs COPY API does not
// expose a remote object length, so S3/MinIO exports report unknown bytes.
func (e *Exporter) recordSuccess(ctx context.Context, table string, day time.Time, rowCount, bytes int64, bytesKnown bool, started time.Time) error {
	if bytes < 0 {
		bytes = 0
	}
	latency := time.Since(started).Milliseconds()
	if latency < 0 {
		latency = 0
	}
	_, err := e.pool.Exec(ctx, `
		UPDATE export_log
		SET status='succeeded', exported_at=now(), attempted_at=COALESCE(attempted_at, now()),
		    row_count=$3, exported_bytes=$4, bytes_known=$5, export_latency_ms=$6, error_code=''
		WHERE tbl=$1 AND day=$2`, table, day, rowCount, bytes, bytesKnown, latency)
	if err != nil {
		return err
	}
	return nil
}
