package analytics

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

	"github.com/bright-interaction/flare/internal/db"
	"github.com/bright-interaction/flare/internal/id"
)

func TestPurgeColdScope(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := &Manager{db: db, cfg: Config{ParquetDir: dir}}
	ctx := context.Background()

	writeParquet := func(day, rows string) {
		d := filepath.Join(dir, "events", "dt="+day)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		f := filepath.ToSlash(filepath.Join(d, "part.parquet"))
		if _, err := db.ExecContext(ctx, "COPY (SELECT * FROM (VALUES "+rows+") t(project_id, org_id, msg)) TO '"+f+"' (FORMAT parquet)"); err != nil {
			t.Fatal(err)
		}
	}
	// day1 mixes projects p1 and p2 (both org o1); day2 has only p2.
	writeParquet("2026-01-01", "('p1','o1','a'),('p2','o1','b')")
	writeParquet("2026-01-02", "('p2','o1','c')")

	count := func(where string) int64 {
		glob := filepath.ToSlash(filepath.Join(dir, "events", "dt=*", "part.parquet"))
		var n int64
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM read_parquet('"+glob+"') "+where).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	// Purge project p1: its rows must vanish, p2's must remain untouched.
	if err := m.PurgeColdScope(ctx, "project_id", "p1"); err != nil {
		t.Fatalf("purge project: %v", err)
	}
	if n := count("WHERE project_id='p1'"); n != 0 {
		t.Errorf("p1 rows survived cold purge: %d", n)
	}
	if n := count("WHERE project_id='p2'"); n != 2 {
		t.Errorf("p2 rows should be untouched, got %d", n)
	}

	// Purge org o1: removes everything; now-empty files are deleted.
	if err := m.PurgeColdScope(ctx, "org_id", "o1"); err != nil {
		t.Fatalf("purge org: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "events", "dt=*", "part.parquet")); len(left) != 0 {
		t.Errorf("empty parquet files should be removed, %d remain", len(left))
	}

	// An unexpected column is rejected (no SQL injection surface), and a nil
	// manager / empty tier is a safe no-op.
	if err := m.PurgeColdScope(ctx, "evil; DROP TABLE", "x"); err == nil {
		t.Error("invalid purge column should be rejected")
	}
	if err := (*Manager)(nil).PurgeColdScope(ctx, "project_id", "p1"); err != nil {
		t.Errorf("nil manager should be a no-op: %v", err)
	}
}

// An erasure must also remove the temporaries an interrupted export (.tmp) or
// an interrupted purge (.purge) left behind: they hold rows too, and under
// archive "off" nothing ever overwrites or prunes them (audit P2-6).
func TestPurgeColdScopeRemovesCrashResidue(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := &Manager{db: db, cfg: Config{ParquetDir: dir}}
	var residue []string
	for _, name := range []string{"part.parquet.tmp", "part.parquet.purge"} {
		d := filepath.Join(dir, "logs", "dt=2026-01-03")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(d, name)
		if err := os.WriteFile(p, []byte("rows of the erased project"), 0o644); err != nil {
			t.Fatal(err)
		}
		residue = append(residue, p)
	}
	if err := m.PurgeColdScope(context.Background(), "project_id", "p1"); err != nil {
		t.Fatal(err)
	}
	for _, p := range residue {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived an erasure purge", filepath.Base(p))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "dt=2026-01-03")); !os.IsNotExist(err) {
		t.Error("the emptied day directory was left behind")
	}
}

// A rewritten file keeps exactly the columns it had. Read with hive
// partitioning, the dt=YYYY-MM-DD path came back as an extra dt column and
// the rewrite wrote it into the archive.
func TestPurgeRewriteDoesNotAddAHivePartitionColumn(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := &Manager{db: db, cfg: Config{ParquetDir: dir}}
	ctx := context.Background()
	d := filepath.Join(dir, "events", "dt=2026-01-01")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.ToSlash(filepath.Join(d, "part.parquet"))
	if _, err := db.ExecContext(ctx, "COPY (SELECT * FROM (VALUES ('p1','o1','a'),('p2','o1','b')) t(project_id, org_id, msg)) TO '"+f+"' (FORMAT parquet)"); err != nil {
		t.Fatal(err)
	}
	if err := m.PurgeColdScope(ctx, "project_id", "p1"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM (DESCRIBE SELECT * FROM read_parquet('"+f+"', hive_partitioning = false))").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("rewritten file has %d columns, want the original 3", n)
	}
}

// Files an older erasure purge rewrote carry a PHYSICAL dt column. A merge
// re-export must not count their rows twice (fix audit C): read without dt,
// they compare equal to the live rows.
func TestMergeWithALegacyDTColumnDoesNotDoubleRows(t *testing.T) {
	adminURL := os.Getenv("FLARE_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("FLARE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	url, pool := legacyDTDB(t, adminURL)
	dir := t.TempDir()
	m, err := Open(ctx, Config{PostgresDSN: url, ParquetDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	e := NewExporter(m, pool)
	d := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -40)
	child := "events_" + d.Format("2006_01_02")
	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE TABLE %s PARTITION OF events FOR VALUES FROM ('%s') TO ('%s')",
		child, d.Format("2006-01-02"), d.AddDate(0, 0, 1).Format("2006-01-02"))); err != nil {
		t.Fatal(err)
	}
	for _, i := range []string{"e1", "e2"} {
		if _, err := pool.Exec(ctx, `INSERT INTO events (id, project_id, org_id, message, received_at) VALUES ($1,'p1','o1','x',$2)`, i, d.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.ExportDay(ctx, "events", d); err != nil {
		t.Fatal(err)
	}
	// Rewrite the file the way the old purge did: with a physical dt column.
	f := filepath.ToSlash(filepath.Join(dir, "events", "dt="+d.Format("2006-01-02"), "part.parquet"))
	if _, err := m.db.ExecContext(ctx, "COPY (SELECT *, DATE '"+d.Format("2006-01-02")+"' AS dt FROM read_parquet('"+f+"', hive_partitioning = false)) TO '"+f+".x' (FORMAT parquet)"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f+".x", f); err != nil {
		t.Fatal(err)
	}
	// One more late row, so the ledger no longer matches and the day merges.
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, project_id, org_id, message, received_at) VALUES ('e3','p1','o1','x',$1)`, d.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := e.ExportDay(ctx, "events", d); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := m.db.QueryRowContext(ctx, "SELECT count(*) FROM read_parquet('"+f+"', hive_partitioning = false)").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("merged archive has %d rows, want 3 (2 archived + 1 late, no doubles)", n)
	}
}

func legacyDTDB(t *testing.T, adminURL string) (string, *pgxpool.Pool) {
	t.Helper()
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
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	url := adminURL[:strings.LastIndex(adminURL, "/")+1] + name
	if q := strings.IndexByte(adminURL, '?'); q >= 0 {
		url = adminURL[:strings.LastIndex(adminURL[:q], "/")+1] + name + adminURL[q:]
	}
	if err := db.RunMigrations(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return url, pool
}
