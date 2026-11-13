package partition_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/marcboeker/go-duckdb/v2"

	"github.com/bright-interaction/flare/internal/analytics"
	"github.com/bright-interaction/flare/internal/db"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/ingest"
	"github.com/bright-interaction/flare/internal/partition"
	"github.com/bright-interaction/flare/internal/retention"
)

// These drive real passes against Postgres and the real DuckDB exporter, to
// prove the export ledger never outlives the archive file it describes (audit
// P0-2). Each one lost data before the fix.

func ledgerDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	adminURL := os.Getenv("FLARE_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("FLARE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "flare_test_" + strings.ToLower(id.New())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	url := adminURL[:strings.LastIndex(adminURL, "/")+1] + name
	if q := strings.IndexByte(adminURL, '?'); q >= 0 {
		url = adminURL[:strings.LastIndex(adminURL[:q], "/")+1] + name + adminURL[q:]
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	if err := db.RunMigrations(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, url
}

func openCold(t *testing.T, url string, pool *pgxpool.Pool) (*analytics.Manager, *analytics.Exporter, string) {
	t.Helper()
	dir := t.TempDir()
	mgr, err := analytics.Open(context.Background(), analytics.Config{PostgresDSN: url, ParquetDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	return mgr, analytics.NewExporter(mgr, pool), dir
}

func fixed(p retention.Policy) partition.PolicyFunc {
	return func(context.Context) (retention.Policy, error) { return p, nil }
}

func parquetRows(t *testing.T, path string) int64 {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		return -1
	}
	d, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var n int64
	if err := d.QueryRow(fmt.Sprintf("SELECT count(*) FROM read_parquet('%s')", path)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func partitionExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func ledger(t *testing.T, pool *pgxpool.Pool, tbl string, day time.Time) (string, int64) {
	t.Helper()
	var status string
	var rows int64
	_ = pool.QueryRow(context.Background(), `SELECT status, row_count FROM export_log WHERE tbl=$1 AND day=$2`, tbl, day).Scan(&status, &rows)
	return status, rows
}

// Hot 30 / archive 30. A pass exports day D, its DROP hits lock_timeout behind
// a dashboard read, and the same pass prunes the archive. The admin then picks
// "archive forever", and the next pass trusted the stale "succeeded" row and
// dropped D with no copy anywhere.
func TestDropLockTimeoutThenPruneNeverLosesTheDay(t *testing.T) {
	pool, url := ledgerDB(t)
	mgr, exp, dir := openCold(t, url, pool)
	ctx := context.Background()
	now := time.Now().UTC()
	d := retention.Today(now).AddDate(0, 0, -31)
	child := "events_" + d.Format("2006_01_02")
	file := filepath.Join(dir, "events", "dt="+d.Format("2006-01-02"), "part.parquet")
	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE TABLE %s PARTITION OF events FOR VALUES FROM ('%s') TO ('%s')",
		child, d.Format("2006-01-02")+" 00:00:00+00", d.AddDate(0, 0, 1).Format("2006-01-02")+" 00:00:00+00")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO events (id, project_id, org_id, message, received_at) VALUES ($1,'p1','o1','kept',$2)`,
			fmt.Sprintf("e%d", i), d.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// Pass 0 pre-creates the rolling window so pass 1's CREATEs short-circuit
	// past the lock held below.
	partition.New(pool, 30, exp, true).WithPolicy(fixed(retention.Policy{HotDays: 365, ColdMode: retention.ColdForever})).OnceForTest(ctx)

	locker, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := locker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE events IN ACCESS SHARE MODE"); err != nil {
		t.Fatal(err)
	}
	cold := mgr
	partition.New(pool, 30, exp, true).WithPolicy(fixed(retention.Policy{HotDays: 30, ColdMode: retention.ColdDays, ColdDays: 30})).
		WithColdPruner(cold).OnceForTest(ctx)
	_ = tx.Rollback(ctx)
	locker.Release()
	if !partitionExists(t, pool, child) {
		t.Fatal("setup: the DROP was expected to time out behind the lock")
	}
	if n := parquetRows(t, file); n != 3 {
		t.Fatalf("archive of %s pruned while its hot partition still exists (rows=%d)", child, n)
	}

	partition.New(pool, 30, exp, true).WithPolicy(fixed(retention.Policy{HotDays: 30, ColdMode: retention.ColdForever})).
		WithColdPruner(cold).OnceForTest(ctx)
	hot := partitionExists(t, pool, child)
	rows := parquetRows(t, file)
	if !hot && rows != 3 {
		t.Fatalf("DATA LOSS: %s dropped under \"archive forever\" with %d rows archived (want 3)", child, rows)
	}
}

// Once the archive of a day is pruned, its export_log row goes with it.
func TestPrunedArchiveDayForgetsItsExportRecord(t *testing.T) {
	pool, url := ledgerDB(t)
	mgr, exp, dir := openCold(t, url, pool)
	ctx := context.Background()
	d := retention.Today(time.Now()).AddDate(0, 0, -40)
	child := "events_" + d.Format("2006_01_02")
	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE TABLE %s PARTITION OF events FOR VALUES FROM ('%s') TO ('%s')",
		child, d.Format("2006-01-02")+" 00:00:00+00", d.AddDate(0, 0, 1).Format("2006-01-02")+" 00:00:00+00")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, project_id, org_id, message, received_at) VALUES ('e1','p1','o1','x',$1)`, d.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	partition.New(pool, 30, exp, true).WithPolicy(fixed(retention.Policy{HotDays: 30, ColdMode: retention.ColdDays, ColdDays: 30})).
		WithColdPruner(mgr).OnceForTest(ctx)
	if partitionExists(t, pool, child) {
		t.Fatal("setup: partition should have been exported and dropped")
	}
	if _, err := os.Stat(filepath.Join(dir, "events", "dt="+d.Format("2006-01-02"))); !os.IsNotExist(err) {
		t.Fatalf("archive day not pruned: %v", err)
	}
	if status, _ := ledger(t, pool, "events", d); status != "" {
		t.Fatalf("export_log still says %q for a pruned archive day", status)
	}
}

// The ledger says "succeeded" but the file is gone (deleted out of band or by
// an older binary's prune). ExportDay must export again, not report success.
func TestExportDayReexportsWhenTheArchiveFileIsGone(t *testing.T) {
	pool, url := ledgerDB(t)
	_, exp, dir := openCold(t, url, pool)
	ctx := context.Background()
	d := retention.Today(time.Now()).AddDate(0, 0, -40)
	child := "events_" + d.Format("2006_01_02")
	file := filepath.Join(dir, "events", "dt="+d.Format("2006-01-02"), "part.parquet")
	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE TABLE %s PARTITION OF events FOR VALUES FROM ('%s') TO ('%s')",
		child, d.Format("2006-01-02")+" 00:00:00+00", d.AddDate(0, 0, 1).Format("2006-01-02")+" 00:00:00+00")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO events (id, project_id, org_id, message, received_at) VALUES ($1,'p1','o1','x',$2)`,
			fmt.Sprintf("e%d", i), d.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := exp.ExportDay(ctx, "events", d); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := exp.ExportDay(ctx, "events", d); err != nil {
		t.Fatal(err)
	}
	if n := parquetRows(t, file); n != 2 {
		t.Fatalf("ExportDay trusted a stale ledger row: archive rows = %d, want 2", n)
	}
	if status, rows := ledger(t, pool, "events", d); status != "succeeded" || rows != 2 {
		t.Fatalf("ledger = %s/%d", status, rows)
	}
}

// hot = MinHotDays = 7 and a UTC clock step back across midnight: day D is
// exported (1 row) and dropped, the step recreates D, ingest accepts a late
// row into it, and the next pass trusted "succeeded" (same row count, 1) and
// dropped the late row unarchived.
func TestBackwardClockStepAtSevenDaysKeepsLateRows(t *testing.T) {
	pool, url := ledgerDB(t)
	_, exp, dir := openCold(t, url, pool)
	ctx := context.Background()
	var clock time.Time
	m := partition.New(pool, 30, exp, true).WithPolicy(fixed(retention.Policy{HotDays: 7, ColdMode: retention.ColdForever}))
	m.SetClockForTest(func() time.Time { return clock })

	x := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	d := x.AddDate(0, 0, -8)
	child := "logs_" + d.Format("2006_01_02")
	file := filepath.Join(dir, "logs", "dt="+d.Format("2006-01-02"), "part.parquet")
	insert := func(ts, nowAtIngest time.Time) {
		t.Helper()
		if at := ingest.ClampTime(ts, nowAtIngest); !at.Equal(ts) {
			t.Fatalf("ingest clamped %s to %s", ts, at)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO logs (id, project_id, org_id, observed_at) VALUES ($1,'p','o',$2)`, id.New(), ts); err != nil {
			t.Fatal(err)
		}
	}

	clock = x.AddDate(0, 0, -1).Add(12 * time.Hour)
	m.OnceForTest(ctx) // pre-creates D
	insert(d.Add(12*time.Hour), clock)
	clock = x.Add(30 * time.Minute)
	m.OnceForTest(ctx) // exports D (1 row) and drops it
	clock = x.Add(-10 * time.Minute)
	m.OnceForTest(ctx) // the step back recreates D
	if !partitionExists(t, pool, child) {
		t.Fatal("setup: the clock step did not recreate the dropped day")
	}
	insert(d.Add(23*time.Hour+55*time.Minute), clock)
	clock = x.Add(6*time.Hour + 30*time.Minute)
	m.OnceForTest(ctx)

	var left int64
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM logs WHERE observed_at >= $1 AND observed_at < $2", d, d.AddDate(0, 0, 1)).Scan(&left)
	if rows := parquetRows(t, file); max(rows, 0)+left < 2 {
		t.Fatalf("late row lost: hot rows=%d, archived rows=%d (want both rows somewhere)", left, rows)
	}
}

// A merge re-export that fails or is killed must not turn the next pass into
// a fresh export that overwrites the archived rows with only the late ones
// (fix audit A). beginAttempt rewrites the ledger to "attempted" before any
// step that can fail, so the retry has to decide to merge from the file.
// hot = 7: day D is archived (5 rows) and dropped, a backward clock step
// recreates it, 2 late rows land in it, and the merge fails once.
func TestFailedOrKilledMergeKeepsTheArchivedRows(t *testing.T) {
	for _, tc := range []struct{ name, failure string }{
		{"merge COPY fails once", "chmod"},
		{"SIGTERM mid-merge leaves attempted", "crash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, url := ledgerDB(t)
			_, exp, dir := openCold(t, url, pool)
			ctx := context.Background()
			var clock time.Time
			m := partition.New(pool, 30, exp, true).WithPolicy(fixed(retention.Policy{HotDays: 7, ColdMode: retention.ColdForever}))
			m.SetClockForTest(func() time.Time { return clock })

			const n, k = 5, 2
			x := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
			d := x.AddDate(0, 0, -8)
			child := "logs_" + d.Format("2006_01_02")
			dayDir := filepath.Join(dir, "logs", "dt="+d.Format("2006-01-02"))
			file := filepath.Join(dayDir, "part.parquet")
			ins := func(ts time.Time) {
				t.Helper()
				if _, err := pool.Exec(ctx, `INSERT INTO logs (id, project_id, org_id, observed_at) VALUES ($1,'p','o',$2)`, id.New(), ts); err != nil {
					t.Fatal(err)
				}
			}
			hotRows := func() int64 {
				var c int64
				_ = pool.QueryRow(ctx, "SELECT count(*) FROM logs WHERE observed_at >= $1 AND observed_at < $2", d, d.AddDate(0, 0, 1)).Scan(&c)
				return c
			}

			clock = x.AddDate(0, 0, -1).Add(12 * time.Hour)
			m.OnceForTest(ctx)
			for i := 0; i < n; i++ {
				ins(d.Add(time.Duration(i+1) * time.Hour))
			}
			clock = x.Add(30 * time.Minute)
			m.OnceForTest(ctx) // exported (5 rows) and dropped
			if partitionExists(t, pool, child) || parquetRows(t, file) != n {
				t.Fatalf("setup: want D archived with %d rows and dropped", n)
			}
			clock = x.Add(-10 * time.Minute)
			m.OnceForTest(ctx) // the step back recreates D
			for i := 0; i < k; i++ {
				ins(d.Add(22*time.Hour + time.Duration(i)*time.Minute))
			}

			clock = x.Add(6*time.Hour + 30*time.Minute)
			switch tc.failure {
			case "chmod":
				// The merge COPY cannot write its .tmp: the pass records "failed".
				if err := os.Chmod(dayDir, 0o555); err != nil {
					t.Fatal(err)
				}
				m.OnceForTest(ctx)
				_ = os.Chmod(dayDir, 0o755)
				if st, _ := ledger(t, pool, "logs", d); st != "failed" {
					t.Fatalf("setup: ledger after the failed merge = %q, want failed", st)
				}
			case "crash":
				// What a process killed after beginAttempt leaves behind.
				if _, err := pool.Exec(ctx, `UPDATE export_log SET status='attempted' WHERE tbl='logs' AND day=$1`, d); err != nil {
					t.Fatal(err)
				}
			}
			m.OnceForTest(ctx) // the retry
			m.OnceForTest(ctx)

			present := max(parquetRows(t, file), 0) + hotRows()
			if present < n+k {
				t.Fatalf("DATA LOSS: %d of %d rows survive (archive %d, hot %d)", present, n+k, parquetRows(t, file), hotRows())
			}
		})
	}
}

// A zero-length or corrupt archive file (a crash before the data reached
// disk) used to make every pass fail the intact check while the ledger said
// "succeeded", so the day was blocked forever and only a WARN said so (fix
// audit D). With the partition still holding the rows, it is re-exported.
func TestUnreadableArchiveFileIsExportedAgain(t *testing.T) {
	pool, url := ledgerDB(t)
	_, exp, dir := openCold(t, url, pool)
	ctx := context.Background()
	d := retention.Today(time.Now()).AddDate(0, 0, -40)
	child := "events_" + d.Format("2006_01_02")
	file := filepath.Join(dir, "events", "dt="+d.Format("2006-01-02"), "part.parquet")
	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE TABLE %s PARTITION OF events FOR VALUES FROM ('%s') TO ('%s')",
		child, d.Format("2006-01-02")+" 00:00:00+00", d.AddDate(0, 0, 1).Format("2006-01-02")+" 00:00:00+00")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO events (id, project_id, org_id, message, received_at) VALUES ($1,'p1','o1','x',$2)`,
			fmt.Sprintf("e%d", i), d.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := exp.ExportDay(ctx, "events", d); err != nil {
		t.Fatal(err)
	}
	for _, corrupt := range [][]byte{nil, []byte("PAR1 not really parquet")} {
		if err := os.WriteFile(file, corrupt, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := exp.ExportDay(ctx, "events", d); err != nil {
			t.Fatalf("an unreadable archive file (%d bytes) blocks the day: %v", len(corrupt), err)
		}
		if n := parquetRows(t, file); n != 2 {
			t.Fatalf("after re-export the archive has %d rows, want 2", n)
		}
	}
}
