// Package partition keeps the time-partitioned telemetry tables healthy: it
// pre-creates daily range partitions ahead of ingest and drops partitions
// past the retention window (retention = DROP PARTITION, instant, no VACUUM
// bloat). The DEFAULT partition created in the migration is the safety net for
// out-of-range timestamps; in normal operation rows land in dated partitions.
package partition

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bright-interaction/flare/internal/metrics"
	"github.com/jackc/pgx/v5/pgxpool"
)

var tables = []string{"events", "logs", "spans", "metrics"}

// behindDays must be >= ingest.MaxBackfillDays so every timestamp ingest accepts
// has a dated partition waiting for it. (Not imported from ingest: partition
// must not depend on the ingest package. The ingest-side test asserts the
// relationship holds.)
const behindDays = 7

// Exporter archives a dated partition to the cold tier before it is dropped.
// nil = no cold tier (just drop). analytics.Exporter satisfies this.
type Exporter interface {
	ExportDay(ctx context.Context, table string, day time.Time) error
}

type Manager struct {
	pool          *pgxpool.Pool
	retentionDays int
	aheadDays     int
	exporter      Exporter
	// requireExport means an aged partition may only be dropped after it is
	// archived. With it set and exporter nil (DuckDB failed to open), dropping
	// would destroy telemetry that was supposed to be archived, so prune refuses
	// and says so on every cycle. The operator opts out with
	// FLARE_ALLOW_DROP_WITHOUT_EXPORT=true when running with no cold tier.
	requireExport bool
}

func New(pool *pgxpool.Pool, retentionDays int, exporter Exporter, requireExport bool) *Manager {
	if retentionDays <= 0 {
		retentionDays = 30
	}
	return &Manager{
		pool: pool, retentionDays: retentionDays, aheadDays: 3,
		exporter: exporter, requireExport: requireExport,
	}
}

// Run does one pass now, then every 6 hours until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	m.once(ctx)
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.once(ctx)
		}
	}
}

func (m *Manager) once(ctx context.Context) {
	for _, tbl := range tables {
		// Cover the backfill window too, not just today forward. Ingest accepts
		// client timestamps up to ingest.MaxBackfillDays old, and a day with no
		// dated partition sends those rows to the DEFAULT partition, which
		// retention never prunes and the exporter never archives.
		for d := -behindDays; d <= m.aheadDays; d++ {
			day := time.Now().UTC().AddDate(0, 0, d)
			if err := ensurePartition(ctx, m.pool, tbl, day); err != nil {
				slog.Warn("partition ensure failed", "table", tbl, "day", day.Format("2006-01-02"), "error", err)
			}
		}
		if err := m.prune(ctx, tbl); err != nil {
			slog.Warn("partition prune failed", "table", tbl, "error", err)
		}
	}
	// The Sentry replay ledger is a plain table because its project/item unique
	// keys cannot include a time partition key. Prune it in bounded batches at
	// the same retention horizon as hot telemetry, and expose failure/backlog.
	if err := m.pruneSentryItemAcceptances(ctx); err != nil {
		metrics.Inc("flare_sentry_dedup_prune", map[string]string{"outcome": "error"})
		slog.Warn("sentry item acceptance prune failed", "error", err)
	}
}

const sentryPruneBatch = 10000

func (m *Manager) pruneSentryItemAcceptances(ctx context.Context) error {
	cutoff := time.Now().UTC().AddDate(0, 0, -m.retentionDays).Truncate(24 * time.Hour)
	// Bound one pass's DB work even if the service has been offline for a long
	// time. The oldest-first index makes every later pass resume where it left.
	const maxBatches = 100
	var total int64
	for i := 0; i < maxBatches; i++ {
		pruned, err := pruneSentryItemAcceptancesBatch(ctx, m.pool, cutoff, sentryPruneBatch)
		if err != nil {
			return err
		}
		total += pruned
		if pruned < sentryPruneBatch {
			metrics.Add("flare_sentry_dedup_pruned", nil, uint64(total))
			return nil
		}
	}
	metrics.Add("flare_sentry_dedup_pruned", nil, uint64(total))
	metrics.Inc("flare_sentry_dedup_prune", map[string]string{"outcome": "backlog"})
	slog.Warn("sentry item acceptance prune backlog remains", "pruned", total, "retention_days", m.retentionDays)
	return nil
}

func pruneSentryItemAcceptancesBatch(ctx context.Context, pool *pgxpool.Pool, cutoff time.Time, limit int) (int64, error) {
	result, err := pool.Exec(ctx, `WITH expired AS (
		SELECT ctid FROM sentry_item_acceptances
		WHERE accepted_at < $1
		ORDER BY accepted_at
		LIMIT $2 FOR UPDATE SKIP LOCKED
	) DELETE FROM sentry_item_acceptances
	WHERE ctid IN (SELECT ctid FROM expired)`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

// ensurePartition creates the dated child partition if absent. Table names come
// from the fixed `tables` allowlist plus a date, so the formatted SQL is safe.
func ensurePartition(ctx context.Context, pool *pgxpool.Pool, table string, day time.Time) error {
	day = day.UTC()
	name := fmt.Sprintf("%s_%s", table, day.Format("2006_01_02"))
	from := day.Format("2006-01-02") + " 00:00:00+00"
	to := day.AddDate(0, 0, 1).Format("2006-01-02") + " 00:00:00+00"
	q := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
		name, table, from, to,
	)
	// Same treatment as the DROP in dropWithLockTimeout, and for the same
	// reason: CREATE TABLE ... PARTITION OF takes ACCESS EXCLUSIVE on the
	// PARENT, so with no deadline it queues behind one in-flight read and then
	// convoys every subsequent read and ingest INSERT behind itself.
	//
	// Measured in the same harness that proved the DROP fix: against a 23s open
	// reader holding AccessShareLock, this CREATE blocked for the full 23s. It
	// only short-circuits when the partition already exists, and once() walks
	// d = -7..+3 across 4 tables every 6h, so at least one genuinely-new
	// partition per table per day runs this path.
	//
	// SET LOCAL requires a transaction block; outside one Postgres warns and
	// discards it, which is exactly how the DROP shipped a no-op timeout.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '5s'"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, q); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (m *Manager) prune(ctx context.Context, table string) error {
	// Configured-but-unavailable cold tier: retention would be a straight DELETE
	// of data the operator expected to be archived. Keep the partitions and make
	// the degraded state loud on every cycle instead.
	if m.requireExport && m.exporter == nil {
		slog.Error("cold tier unavailable; refusing to drop aged partitions because they were never archived. Fix analytics/DuckDB, or set FLARE_ALLOW_DROP_WITHOUT_EXPORT=true to prune without archiving",
			"table", table)
		return nil
	}
	names, err := childPartitions(ctx, m.pool, table)
	if err != nil {
		return err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -m.retentionDays).Truncate(24 * time.Hour)
	for _, name := range names {
		suffix := name[len(table)+1:] // YYYY_MM_DD
		d, err := time.Parse("2006_01_02", suffix)
		if err != nil {
			continue
		}
		if d.Before(cutoff) {
			// Export-then-drop: a partition is only dropped after its rows are
			// durably in the Parquet cold tier. If export fails, keep the
			// partition and retry next cycle (no data loss).
			if m.exporter != nil {
				if err := m.exporter.ExportDay(ctx, table, d); err != nil {
					slog.Warn("partition export failed; keeping partition for retry", "partition", name, "error", err)
					continue
				}
			}
			// DROP takes ACCESS EXCLUSIVE on the parent, so without a
			// lock_timeout it queues ahead of every read and every ingest INSERT
			// and holds them until it gets the lock. Give up quickly instead and
			// retry on the next cycle; the partition is already past retention,
			// so a few hours' delay costs nothing.
			if err := m.dropWithLockTimeout(ctx, name); err != nil {
				slog.Warn("drop partition failed; will retry next cycle", "partition", name, "error", err)
				continue
			}
			slog.Info("dropped expired partition", "partition", name)
		}
	}
	return nil
}

// dropWithLockTimeout drops a partition under a short lock_timeout, inside an
// explicit TRANSACTION.
//
// The transaction is the entire point, and its absence made this a no-op. SET
// LOCAL only applies within a transaction block: run outside one, Postgres
// emits `WARNING: SET LOCAL can only be used in transaction blocks` and
// discards it, because each statement is then its own implicit transaction that
// ends before the next one begins. So the DROP that followed ran with the
// default lock_timeout of 0, meaning wait forever, which is exactly the
// behaviour the timeout was added to prevent.
//
// That matters because DROP TABLE takes ACCESS EXCLUSIVE on the parent. Without
// a deadline it queues behind any in-flight read and then convoys every
// subsequent read AND ingest INSERT behind itself for as long as that read
// runs. A dashboard query over a wide time range is enough to stall ingest.
//
// With the timeout actually in force the DROP gives up after 5s and the caller
// logs and retries on the next cycle; the partition is already past retention,
// so a few hours' delay costs nothing.
func (m *Manager) dropWithLockTimeout(ctx context.Context, name string) error {
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	// Rollback is a no-op once Commit has run; on any early return it releases
	// the lock rather than holding it to the end of the pooled connection.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '5s'"); err != nil {
		return err
	}
	// Table name comes from the fixed `tables` allowlist plus a date matched by
	// the childPartitions regex, so the formatted SQL is safe.
	if _, err := tx.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", name)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// childPartitions returns the dated child partition names of a parent table.
func childPartitions(ctx context.Context, p *pgxpool.Pool, table string) ([]string, error) {
	rows, err := p.Query(ctx, `
		SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class parent ON parent.oid = i.inhparent
		WHERE parent.relname = $1
		  AND c.relname ~ ('^' || $1 || '_[0-9]{4}_[0-9]{2}_[0-9]{2}$')`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}
