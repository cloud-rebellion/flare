// Package partition keeps the time-partitioned telemetry tables healthy: it
// pre-creates daily range partitions ahead of ingest and drops partitions
// past the retention window (retention = DROP PARTITION, instant, no VACUUM
// bloat). The DEFAULT partition created in the migration is the safety net for
// out-of-range timestamps; in normal operation rows land in dated partitions.
//
// The retention policy is read on EVERY pass (see WithPolicy), so a value saved
// in the dashboard takes effect on the next pass without a restart.
package partition

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bright-interaction/flare/internal/metrics"
	"github.com/bright-interaction/flare/internal/retention"
	"github.com/jackc/pgx/v5/pgxpool"
)

var tables = []string{"events", "logs", "spans", "metrics"}

// Tables is the fixed list of partitioned telemetry parents.
func Tables() []string { return append([]string(nil), tables...) }

// behindDays must be >= ingest.MaxBackfillDays so every timestamp ingest accepts
// has a dated partition waiting for it. (Not imported from ingest: partition
// must not depend on the ingest package. The ingest-side test asserts the
// relationship holds.) retention.MinHotDays must be >= behindDays, or a pass
// would recreate a backfill partition and drop it again; partition's tests pin
// that.
const behindDays = 7

// Exporter archives a dated partition to the cold tier before it is dropped.
// nil = no cold tier (just drop). analytics.Exporter satisfies this.
type Exporter interface {
	ExportDay(ctx context.Context, table string, day time.Time) error
}

// ColdPruner deletes archived days the policy no longer keeps.
// analytics.Manager satisfies this. It returns ErrColdPruneUnsupported (wrapped
// or not) when the archive lives somewhere it cannot delete from (S3).
type ColdPruner interface {
	PruneCold(ctx context.Context, expired func(table string, day time.Time) bool) (ColdPruneResult, error)
}

// ColdPruneResult counts what one cold prune removed.
type ColdPruneResult struct {
	Days  int   `json:"days"`
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// Pruned names every archive day that was deleted, so the export ledger
	// can forget it. Not part of the settings-page status.
	Pruned []PrunedDay `json:"-"`
}

// PrunedDay is one table's archive day removed by a cold prune.
type PrunedDay struct {
	Table string
	Day   time.Time
}

// ErrColdPruneUnsupported is what a ColdPruner returns for an archive it cannot
// delete from. The archive is left untouched.
var ErrColdPruneUnsupported = errors.New("cold-tier pruning is not supported for this archive")

// PolicyFunc returns the effective retention policy. It is called once per pass.
// An error means "do not know", and the pass prunes NOTHING: falling back to a
// default could be shorter than what an admin stored.
type PolicyFunc func(ctx context.Context) (retention.Policy, error)

// Status is what the last pass did, for the settings page.
type Status struct {
	LastPassAt     time.Time       `json:"last_pass_at"`
	Policy         string          `json:"policy,omitempty"`
	PolicyError    bool            `json:"policy_error"`
	HotDropped     int             `json:"hot_partitions_dropped"`
	HotRefused     bool            `json:"hot_prune_paused"`
	ColdPruned     ColdPruneResult `json:"cold_pruned"`
	ColdPruneError bool            `json:"cold_prune_error"`
	ColdPruneNoop  string          `json:"cold_prune_skipped,omitempty"`
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
	// FLARE_ALLOW_DROP_WITHOUT_EXPORT=true when running with no cold tier, or by
	// choosing "do not archive" in the dashboard.
	requireExport bool
	policy        PolicyFunc
	cold          ColdPruner
	now           func() time.Time

	mu     sync.Mutex
	status Status
}

func New(pool *pgxpool.Pool, retentionDays int, exporter Exporter, requireExport bool) *Manager {
	if retentionDays <= 0 {
		retentionDays = 30
	}
	m := &Manager{
		pool: pool, retentionDays: retentionDays, aheadDays: 3,
		exporter: exporter, requireExport: requireExport, now: time.Now,
	}
	m.policy = func(context.Context) (retention.Policy, error) {
		return retention.Policy{HotDays: m.retentionDays, ColdMode: retention.ColdForever}, nil
	}
	return m
}

// WithPolicy makes every pass read the effective policy from fn instead of the
// fixed retentionDays given to New.
func (m *Manager) WithPolicy(fn PolicyFunc) *Manager {
	if fn != nil {
		m.policy = fn
	}
	return m
}

// WithColdPruner enables archive pruning for policies in cold mode "days".
func (m *Manager) WithColdPruner(p ColdPruner) *Manager {
	m.cold = p
	return m
}

// LastStatus returns a copy of what the most recent pass did.
func (m *Manager) LastStatus() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
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
	now := m.now()
	st := Status{LastPassAt: now.UTC()}
	// Pre-creation never depends on the policy: a failed policy read must not
	// leave tomorrow's ingest without a partition.
	for _, tbl := range tables {
		// Cover the backfill window too, not just today forward. Ingest accepts
		// client timestamps up to ingest.MaxBackfillDays old, and a day with no
		// dated partition sends those rows to the DEFAULT partition, which
		// retention never prunes and the exporter never archives.
		for d := -behindDays; d <= m.aheadDays; d++ {
			day := now.UTC().AddDate(0, 0, d)
			if err := ensurePartition(ctx, m.pool, tbl, day); err != nil {
				slog.Warn("partition ensure failed", "table", tbl, "day", day.Format("2006-01-02"), "error", err)
			}
		}
	}

	policy, err := m.policy(ctx)
	if err != nil {
		// Fail closed: nothing is dropped or pruned on a pass that cannot say
		// what the retention is. Partitions accumulate until the read works.
		metrics.Inc("flare_retention_policy", map[string]string{"outcome": "error"})
		slog.Error("retention policy unavailable; skipping every drop and prune this pass", "error", err)
		st.PolicyError = true
		m.setStatus(st)
		return
	}
	if policy.HotDays < 1 {
		policy.HotDays = m.retentionDays
	}
	st.Policy = policy.Describe()
	if !policy.Archives() {
		slog.Warn("retention: archiving is OFF by explicit choice; aged partitions are dropped without a Parquet export",
			"hot_retention_days", policy.HotDays)
	}

	for _, tbl := range tables {
		dropped, refused, err := m.prune(ctx, tbl, policy, now)
		st.HotDropped += dropped
		st.HotRefused = st.HotRefused || refused
		if err != nil {
			slog.Warn("partition prune failed", "table", tbl, "error", err)
		}
	}
	// The Sentry replay ledger is a plain table because its project/item unique
	// keys cannot include a time partition key. Prune it in bounded batches at
	// the same retention horizon as hot telemetry, and expose failure/backlog.
	if err := m.pruneSentryItemAcceptances(ctx, policy.HotDays, now); err != nil {
		metrics.Inc("flare_sentry_dedup_prune", map[string]string{"outcome": "error"})
		slog.Warn("sentry item acceptance prune failed", "error", err)
	}

	m.pruneCold(ctx, m.archivePrunePolicy(ctx, policy, &st), now, &st)
	m.setStatus(st)
}

func (m *Manager) setStatus(st Status) {
	m.mu.Lock()
	m.status = st
	m.mu.Unlock()
}

// archivePrunePolicy re-reads the policy right before the archive prune. The
// exports and drops above can run for hours after a large reduction, and an
// admin who reverts in that time must not lose the archive to the policy this
// pass started with: the prune applies whichever keeps more. A failed re-read
// means no prune this pass. Only "days" prunes, so only then is it re-read.
func (m *Manager) archivePrunePolicy(ctx context.Context, start retention.Policy, st *Status) retention.Policy {
	if start.ColdMode != retention.ColdDays {
		return start
	}
	now, err := m.policy(ctx)
	if err != nil {
		st.ColdPruneError = true
		metrics.Inc("flare_cold_prune", map[string]string{"outcome": "error"})
		slog.Error("retention: archive pruning skipped, the policy could not be re-read", "error", err)
		return retention.Policy{HotDays: start.HotDays, ColdMode: retention.ColdForever}
	}
	return retention.ConservativeForArchivePrune(start, now)
}

// pruneCold deletes archive days the policy no longer keeps. Only cold mode
// "days" prunes; "forever" keeps everything and "off" leaves what is already
// archived alone (it stops new exports, it is not an archive wipe).
func (m *Manager) pruneCold(ctx context.Context, policy retention.Policy, now time.Time, st *Status) {
	if policy.ColdMode != retention.ColdDays {
		return
	}
	if m.cold == nil {
		st.ColdPruneNoop = "cold tier unavailable"
		slog.Warn("retention: archive pruning skipped, the cold tier is not open", "cold_retention_days", policy.ColdDays)
		return
	}
	// Never prune the archive of a day whose hot partition still exists. That
	// partition is past retention only because its DROP failed (lock timeout)
	// or a clock step recreated it, and the next pass will export and drop it
	// on the strength of this very file. Fail closed if the list is unreadable.
	live := map[string]bool{}
	for _, tbl := range tables {
		names, err := childPartitions(ctx, m.pool, tbl)
		if err != nil {
			st.ColdPruneError = true
			metrics.Inc("flare_cold_prune", map[string]string{"outcome": "error"})
			slog.Error("retention: archive pruning skipped, the hot partitions could not be listed", "table", tbl, "error", err)
			return
		}
		for _, n := range names {
			live[n] = true
		}
	}
	res, err := m.cold.PruneCold(ctx, func(table string, day time.Time) bool {
		if live[table+"_"+day.UTC().Format("2006_01_02")] {
			return false
		}
		return retention.ShouldPruneCold(now, day, policy)
	})
	m.forgetPrunedExports(ctx, res.Pruned)
	st.ColdPruned = res
	switch {
	case errors.Is(err, ErrColdPruneUnsupported):
		st.ColdPruneNoop = "unsupported for this archive"
		metrics.Inc("flare_cold_prune", map[string]string{"outcome": "unsupported"})
		slog.Warn("retention: archive pruning is not supported for this cold tier (S3); set a bucket lifecycle rule instead",
			"cold_retention_days", policy.ColdDays)
	case err != nil:
		st.ColdPruneError = true
		metrics.Inc("flare_cold_prune", map[string]string{"outcome": "error"})
		slog.Error("retention: archive pruning failed; remaining days kept for the next pass", "error", err,
			"pruned_days", res.Days, "pruned_files", res.Files)
	default:
		metrics.Inc("flare_cold_prune", map[string]string{"outcome": "ok"})
		if res.Days > 0 {
			slog.Info("retention: pruned archive days", "days", res.Days, "files", res.Files, "bytes", res.Bytes,
				"cold_retention_days", policy.ColdDays)
		}
	}
}

// forgetPrunedExports deletes the export_log rows of archive days that no
// longer exist, so "succeeded" never outlives the file it describes. The
// exporter also re-checks the file before trusting the row, so a failure here
// is logged rather than fatal.
func (m *Manager) forgetPrunedExports(ctx context.Context, days []PrunedDay) {
	if len(days) == 0 {
		return
	}
	// The files are already deleted, so this must finish even when the pass
	// was cancelled (SIGTERM on deploy): nothing revisits a day whose
	// directory no longer exists. Bounded, and detached from the pass.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	for _, d := range days {
		if _, err := m.pool.Exec(ctx, `DELETE FROM export_log WHERE tbl = $1 AND day = $2`, d.Table, d.Day.UTC()); err != nil {
			slog.Error("retention: could not clear the export record of a pruned archive day", "table", d.Table,
				"day", d.Day.UTC().Format("2006-01-02"), "error", err)
		}
	}
}

const sentryPruneBatch = 10000

func (m *Manager) pruneSentryItemAcceptances(ctx context.Context, hotDays int, now time.Time) error {
	cutoff := retention.Cutoff(now, hotDays)
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
	slog.Warn("sentry item acceptance prune backlog remains", "pruned", total, "retention_days", hotDays)
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

// prune drops the table's partitions that are past hot retention, exporting
// each one first unless archiving is off. It returns how many it dropped and
// whether it refused to run because the required cold tier is unavailable.
func (m *Manager) prune(ctx context.Context, table string, policy retention.Policy, now time.Time) (int, bool, error) {
	archive := policy.Archives()
	// Configured-but-unavailable cold tier: retention would be a straight DELETE
	// of data the operator expected to be archived. Keep the partitions and make
	// the degraded state loud on every cycle instead. Cold mode "off" is the
	// operator saying, in the dashboard, that dropping unarchived is intended.
	if archive && m.requireExport && m.exporter == nil {
		slog.Error("cold tier unavailable; refusing to drop aged partitions because they were never archived. Fix analytics/DuckDB, set FLARE_ALLOW_DROP_WITHOUT_EXPORT=true, or choose \"do not archive\" in Settings",
			"table", table)
		return 0, true, nil
	}
	names, err := childPartitions(ctx, m.pool, table)
	if err != nil {
		return 0, false, err
	}
	dropped := 0
	archived := archive && m.exporter != nil
	for _, p := range expiredPartitions(table, names, now, policy.HotDays) {
		// Export-then-drop: a partition is only dropped after its rows are
		// durably in the Parquet cold tier. If export fails, keep the
		// partition and retry next cycle (no data loss). ExportDay returns
		// nil only when the archive verifiably holds the partition (a ledger
		// row is re-checked against the file and the live row count), so
		// "archived" in the log below is a fact, not the ledger's word.
		if archived {
			if err := m.exporter.ExportDay(ctx, table, p.day); err != nil {
				slog.Warn("partition export failed; keeping partition for retry", "partition", p.name, "error", err)
				continue
			}
		}
		// DROP takes ACCESS EXCLUSIVE on the parent, so without a
		// lock_timeout it queues ahead of every read and every ingest INSERT
		// and holds them until it gets the lock. Give up quickly instead and
		// retry on the next cycle; the partition is already past retention,
		// so a few hours' delay costs nothing.
		if err := m.dropWithLockTimeout(ctx, p.name); err != nil {
			slog.Warn("drop partition failed; will retry next cycle", "partition", p.name, "error", err)
			continue
		}
		dropped++
		slog.Info("dropped expired partition", "partition", p.name, "archived", archived)
	}
	return dropped, false, nil
}

type datedPartition struct {
	name string
	day  time.Time
}

// expiredPartitions picks, from a table's child partition names, the ones past
// hot retention. Names that do not parse as <table>_YYYY_MM_DD are ignored, and
// retention.ShouldDropHot never selects today or a future day.
func expiredPartitions(table string, names []string, now time.Time, hotDays int) []datedPartition {
	var out []datedPartition
	prefix := table + "_"
	for _, name := range names {
		if len(name) != len(prefix)+len("2006_01_02") || name[:len(prefix)] != prefix {
			continue
		}
		d, err := time.Parse("2006_01_02", name[len(prefix):])
		if err != nil {
			continue
		}
		if retention.ShouldDropHot(now, d, hotDays) {
			out = append(out, datedPartition{name: name, day: d})
		}
	}
	return out
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
