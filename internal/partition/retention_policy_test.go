package partition

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/retention"
)

// The bound the dashboard enforces must cover the backfill window this package
// pre-creates, or a pass recreates a partition ingest still writes into and
// then drops it.
func TestMinHotRetentionCoversTheBackfillWindow(t *testing.T) {
	if retention.MinHotDays < behindDays {
		t.Fatalf("retention.MinHotDays (%d) < behindDays (%d)", retention.MinHotDays, behindDays)
	}
}

func TestExpiredPartitionsNeverSelectTodayOrFuture(t *testing.T) {
	now := time.Date(2026, 11, 8, 23, 59, 0, 0, time.UTC)
	names := []string{
		"events_2026_10_31", // first day past a 7 day window
		"events_2026_11_01", // oldest kept day
		"events_2026_11_08", // today
		"events_2026_11_09", // pre-created future
		"events_2026_11_11",
		"events_default",      // the DEFAULT partition is never a dated child
		"events_2026_13_01",   // not a date
		"logs_2026_01_01",     // another table's name
		"events_2026_01_01_x", // wrong shape
	}
	got := map[string]bool{}
	for _, p := range expiredPartitions("events", names, now, 7) {
		got[p.name] = true
	}
	if len(got) != 1 || !got["events_2026_10_31"] {
		t.Fatalf("hot=7 selected %v, want only events_2026_10_31", got)
	}
	// Even a nonsense window cannot reach today or the pre-created future.
	for _, hot := range []int{0, -1, -400} {
		for _, p := range expiredPartitions("events", names, now, hot) {
			if !p.day.Before(retention.Today(now)) {
				t.Fatalf("hot=%d selected %s", hot, p.name)
			}
		}
	}
}

// recordingExporter records every export and fails the days it is told to.
type recordingExporter struct {
	mu       sync.Mutex
	exported []string
	fail     map[string]bool
}

func (e *recordingExporter) ExportDay(_ context.Context, table string, day time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	k := table + "_" + day.Format("2006_01_02")
	if e.fail[k] {
		return errors.New("export failed")
	}
	e.exported = append(e.exported, k)
	return nil
}

type fakeColdPruner struct {
	calls   int
	checked []time.Time
	err     error
}

func (f *fakeColdPruner) PruneCold(_ context.Context, expired func(string, time.Time) bool) (ColdPruneResult, error) {
	f.calls++
	var res ColdPruneResult
	for _, d := range f.checked {
		if expired("events", d) {
			res.Days++
		}
	}
	return res, f.err
}

func newPartitionTestDB(t *testing.T) *pgxpool.Pool {
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
	database := "flare_test_" + strings.ToLower(id.New())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	path := adminURL[:strings.LastIndex(adminURL, "/")+1] + database
	if query := strings.IndexByte(adminURL, '?'); query >= 0 {
		path = adminURL[:strings.LastIndex(adminURL[:query], "/")+1] + database + adminURL[query:]
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)") })
	if err := db.RunMigrations(ctx, path); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func eventPartitions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	names, err := childPartitions(context.Background(), pool, "events")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestPassReadsThePolicyEveryRun drives real passes against Postgres: the
// policy is re-read per pass, lowering it drops on the next pass, a policy
// error drops nothing, today and the pre-created future survive, and archive
// mode decides whether the exporter runs.
func TestPassReadsThePolicyEveryRun(t *testing.T) {
	pool := newPartitionTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := func(daysAgo int) string { return "events_" + now.AddDate(0, 0, -daysAgo).Format("2006_01_02") }
	for _, ago := range []int{9, 20, 40} {
		if err := ensurePartition(ctx, pool, "events", now.AddDate(0, 0, -ago)); err != nil {
			t.Fatal(err)
		}
	}

	exp := &recordingExporter{fail: map[string]bool{}}
	var policy retention.Policy
	var policyErr error
	reads := 0
	m := New(pool, 30, exp, true).WithPolicy(func(context.Context) (retention.Policy, error) {
		reads++
		return policy, policyErr
	})
	cold := &fakeColdPruner{}
	m.WithColdPruner(cold)

	// Pass 1: 30 days, archive forever. Only the 40-day-old partition goes,
	// exported first; the cold pruner is not consulted in forever mode.
	policy = retention.Policy{HotDays: 30, ColdMode: retention.ColdForever}
	m.once(ctx)
	names := eventPartitions(t, pool)
	if contains(names, old(40)) || !contains(names, old(20)) || !contains(names, old(9)) {
		t.Fatalf("pass 1 at 30d left %v", names)
	}
	if len(exp.exported) == 0 || !contains(exp.exported, old(40)) {
		t.Fatalf("partition dropped without export: exported=%v", exp.exported)
	}
	if cold.calls != 0 {
		t.Fatal("forever mode must not prune the archive")
	}

	// Pass 2: the policy read fails. Nothing is dropped, partitions are still
	// pre-created, and the status says so.
	policy = retention.Policy{HotDays: 7, ColdMode: retention.ColdOff}
	policyErr = errors.New("db down")
	m.once(ctx)
	names = eventPartitions(t, pool)
	if !contains(names, old(20)) || !contains(names, old(9)) {
		t.Fatalf("a failed policy read dropped partitions: %v", names)
	}
	if !m.LastStatus().PolicyError {
		t.Fatal("status must report the policy error")
	}

	// Pass 3: lowered to 7 days with archiving off. Takes effect now, with no
	// restart: 20 and 9 days old go, WITHOUT export. Today and the future stay.
	policyErr = nil
	exp.exported = nil
	m.once(ctx)
	names = eventPartitions(t, pool)
	if contains(names, old(20)) || contains(names, old(9)) {
		t.Fatalf("lowered policy not applied on the next pass: %v", names)
	}
	if len(exp.exported) != 0 {
		t.Fatalf("archive off must not export: %v", exp.exported)
	}
	for d := -7; d <= 3; d++ {
		want := "events_" + now.AddDate(0, 0, d).Format("2006_01_02")
		if !contains(names, want) {
			t.Fatalf("%s missing after pass 3 (today/future/backfill must be kept): %v", want, names)
		}
	}
	if reads != 3 {
		t.Fatalf("policy read %d times over 3 passes, want once per pass", reads)
	}
	if st := m.LastStatus(); st.HotDropped == 0 || st.Policy != "hot 7d, no archive" {
		t.Fatalf("status = %+v", st)
	}
}

func TestArchivingPolicyRefusesWithoutAColdTier(t *testing.T) {
	pool := newPartitionTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	name := "events_" + now.AddDate(0, 0, -40).Format("2006_01_02")
	if err := ensurePartition(ctx, pool, "events", now.AddDate(0, 0, -40)); err != nil {
		t.Fatal(err)
	}
	policy := retention.Policy{HotDays: 7, ColdMode: retention.ColdDays, ColdDays: 30}
	m := New(pool, 30, nil, true).WithPolicy(func(context.Context) (retention.Policy, error) { return policy, nil })

	// Archiving required, exporter missing: keep everything.
	m.once(ctx)
	if !contains(eventPartitions(t, pool), name) {
		t.Fatal("dropped an unarchived partition while archiving is required")
	}
	if st := m.LastStatus(); !st.HotRefused || st.ColdPruneNoop == "" {
		t.Fatalf("status must report the pause and the missing cold tier: %+v", st)
	}

	// The explicit "do not archive" choice is what allows the drop.
	policy = retention.Policy{HotDays: 7, ColdMode: retention.ColdOff}
	m.once(ctx)
	if contains(eventPartitions(t, pool), name) {
		t.Fatal("archive off must drop without a cold tier")
	}
}

func TestFailedExportKeepsThePartition(t *testing.T) {
	pool := newPartitionTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	name := "events_" + now.AddDate(0, 0, -40).Format("2006_01_02")
	if err := ensurePartition(ctx, pool, "events", now.AddDate(0, 0, -40)); err != nil {
		t.Fatal(err)
	}
	exp := &recordingExporter{fail: map[string]bool{name: true}}
	cold := &fakeColdPruner{checked: []time.Time{now.AddDate(0, 0, -31), now.AddDate(0, 0, -29)}}
	m := New(pool, 30, exp, true).WithPolicy(func(context.Context) (retention.Policy, error) {
		return retention.Policy{HotDays: 7, ColdMode: retention.ColdDays, ColdDays: 30}, nil
	}).WithColdPruner(cold)
	m.once(ctx)
	if !contains(eventPartitions(t, pool), name) {
		t.Fatal("partition dropped after its export failed")
	}
	// Cold pruning ran with the policy's cutoff: 31 days old is past 30, 29 is not.
	if cold.calls != 1 || m.LastStatus().ColdPruned.Days != 1 {
		t.Fatalf("cold prune calls=%d status=%+v", cold.calls, m.LastStatus())
	}

	// S3-style refusal is reported as unsupported, not as an error.
	cold.err = ErrColdPruneUnsupported
	m.once(ctx)
	if st := m.LastStatus(); st.ColdPruneError || st.ColdPruneNoop == "" {
		t.Fatalf("unsupported archive must be reported as skipped: %+v", st)
	}
}

// A pass reads the policy, then exports and drops (which can take hours
// after a large reduction), then prunes the archive. A revert saved in
// between must win: the prune re-reads the policy and applies whichever of
// the two keeps more archive (audit P1-3).
func TestColdPruneUsesTheMoreConservativePolicy(t *testing.T) {
	pool := newPartitionTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := []time.Time{now.AddDate(0, 0, -400), now.AddDate(0, 0, -100), now.AddDate(0, 0, -40)}
	for _, tc := range []struct {
		name          string
		first, second retention.Policy
		wantPruned    int
	}{
		{"reverted to forever mid-pass", retention.Policy{HotDays: 30, ColdMode: retention.ColdDays, ColdDays: 30}, retention.Policy{HotDays: 30, ColdMode: retention.ColdForever}, 0},
		{"switched to off mid-pass", retention.Policy{HotDays: 30, ColdMode: retention.ColdDays, ColdDays: 30}, retention.Policy{HotDays: 30, ColdMode: retention.ColdOff}, 0},
		{"raised mid-pass", retention.Policy{HotDays: 30, ColdMode: retention.ColdDays, ColdDays: 30}, retention.Policy{HotDays: 30, ColdMode: retention.ColdDays, ColdDays: 365}, 1},
		{"lowered mid-pass keeps the first", retention.Policy{HotDays: 30, ColdMode: retention.ColdDays, ColdDays: 365}, retention.Policy{HotDays: 30, ColdMode: retention.ColdDays, ColdDays: 30}, 1},
		{"unchanged", retention.Policy{HotDays: 30, ColdMode: retention.ColdDays, ColdDays: 30}, retention.Policy{HotDays: 30, ColdMode: retention.ColdDays, ColdDays: 30}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			m := New(pool, 30, &recordingExporter{fail: map[string]bool{}}, true).WithPolicy(func(context.Context) (retention.Policy, error) {
				reads++
				if reads == 1 {
					return tc.first, nil
				}
				return tc.second, nil
			})
			cold := &fakeColdPruner{checked: old}
			m.WithColdPruner(cold)
			m.once(ctx)
			if got := m.LastStatus().ColdPruned.Days; got != tc.wantPruned {
				t.Fatalf("pruned %d archive days, want %d (policy %s then %s)", got, tc.wantPruned, tc.first.Describe(), tc.second.Describe())
			}
		})
	}
}

// If the re-read fails, the archive is not pruned on this pass.
func TestColdPruneSkipsWhenThePolicyReReadFails(t *testing.T) {
	pool := newPartitionTestDB(t)
	reads := 0
	m := New(pool, 30, &recordingExporter{fail: map[string]bool{}}, true).WithPolicy(func(context.Context) (retention.Policy, error) {
		reads++
		if reads == 1 {
			return retention.Policy{HotDays: 30, ColdMode: retention.ColdDays, ColdDays: 30}, nil
		}
		return retention.Policy{}, errors.New("db down")
	})
	cold := &fakeColdPruner{checked: []time.Time{time.Now().AddDate(0, 0, -400)}}
	m.WithColdPruner(cold)
	m.once(context.Background())
	if cold.calls != 0 || !m.LastStatus().ColdPruneError {
		t.Fatalf("cold prune calls = %d, status = %+v; want no prune and an error flag", cold.calls, m.LastStatus())
	}
}

// The archive files are already gone when the ledger rows are cleared, so a
// pass cancelled at that moment (SIGTERM on deploy) must still clear them:
// nothing revisits a day whose directory no longer exists (fix audit E).
func TestPrunedExportRecordsAreClearedEvenAfterCancellation(t *testing.T) {
	pool := newPartitionTestDB(t)
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(context.Background(), `INSERT INTO export_log (tbl, day, status, attempted_at, exported_bytes, bytes_known, export_latency_ms, error_code, row_count)
		VALUES ('events', $1, 'succeeded', now(), 10, true, 1, '', 3)`, day); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	New(pool, 30, nil, false).forgetPrunedExports(ctx, []PrunedDay{{Table: "events", Day: day}})
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM export_log WHERE tbl='events' AND day=$1`, day).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("a cancelled pass left the export record of a pruned archive day behind")
	}
}
