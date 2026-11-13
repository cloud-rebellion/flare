package analytics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/partition"
	"github.com/bright-interaction/flare/internal/retention"
)

func writeColdFile(t *testing.T, dir, table, day, name string, size int) string {
	t.Helper()
	d := filepath.Join(dir, table, "dt="+day)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, name)
	if err := os.WriteFile(p, []byte(strings.Repeat("x", size)), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestPruneColdDeletesOnlyExpiredDays(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{cfg: Config{ParquetDir: dir}}
	ctx := context.Background()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	policy := retention.Policy{HotDays: 7, ColdMode: retention.ColdDays, ColdDays: 30}
	expired := func(_ string, d time.Time) bool { return retention.ShouldPruneCold(now, d, policy) }

	oldEvents := writeColdFile(t, dir, "events", "2026-10-01", "part.parquet", 100)
	writeColdFile(t, dir, "events", "2026-10-01", "part.parquet.tmp", 5)          // crash leftover
	boundary := writeColdFile(t, dir, "events", "2026-10-08", "part.parquet", 10) // first expired day
	kept := writeColdFile(t, dir, "events", "2026-10-09", "part.parquet", 10)     // oldest kept day
	oldLogs := writeColdFile(t, dir, "logs", "2026-09-01", "part.parquet", 50)
	foreign := writeColdFile(t, dir, "spans", "2026-09-01", "part.parquet", 7)
	writeColdFile(t, dir, "spans", "2026-09-01", "notes.txt", 1) // not Flare's file
	// Look-alike names are never touched.
	odd := filepath.Join(dir, "events", "dt=2026-10-01-copy")
	if err := os.MkdirAll(odd, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlinked day directory is not followed or removed.
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "part.parquet"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "metrics", "dt=2020-01-01")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	res, err := m.PruneCold(ctx, expired)
	if err == nil || !strings.Contains(err.Error(), "notes.txt") {
		t.Fatalf("a day with unexpected contents must be reported, got %v", err)
	}
	if exists(oldEvents) || exists(boundary) || exists(oldLogs) {
		t.Fatal("expired days were not deleted")
	}
	if !exists(kept) {
		t.Fatal("a day inside cold retention was deleted")
	}
	if !exists(filepath.Join(foreign, "part.parquet")) || !exists(filepath.Join(foreign, "notes.txt")) {
		t.Fatal("a day with foreign files must be left entirely in place")
	}
	if !exists(odd) || !exists(filepath.Join(target, "part.parquet")) || !exists(link) {
		t.Fatal("non-matching or symlinked entries were touched")
	}
	if res.Days != 3 || res.Files != 4 || res.Bytes != 165 {
		t.Fatalf("result = %+v, want 3 days, 4 files, 165 bytes", res)
	}

	inv, err := m.ColdInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var days []string
	for _, u := range inv {
		days = append(days, u.Table+"/"+u.Day.Format("2006-01-02"))
	}
	if strings.Join(days, ",") != "events/2026-10-09,spans/2026-09-01" {
		t.Fatalf("inventory after prune = %v", days)
	}
}

func TestPruneColdEmptyArchiveIsANoop(t *testing.T) {
	m := &Manager{cfg: Config{ParquetDir: filepath.Join(t.TempDir(), "never-created")}}
	res, err := m.PruneCold(context.Background(), func(string, time.Time) bool { return true })
	if err != nil || res.Days != 0 {
		t.Fatalf("empty archive: %+v %v", res, err)
	}
}

func TestPruneColdRefusesS3AndClosed(t *testing.T) {
	s3 := &Manager{cfg: Config{ParquetDir: t.TempDir(), S3Endpoint: "minio:9000"}}
	if _, err := s3.PruneCold(context.Background(), func(string, time.Time) bool { return true }); !errors.Is(err, partition.ErrColdPruneUnsupported) {
		t.Fatalf("S3 prune = %v, want ErrColdPruneUnsupported", err)
	}
	if s3.ColdPruneSupported() || s3.ColdKind() != "s3" {
		t.Fatal("S3 archive must report prune unsupported")
	}
	if _, err := (*Manager)(nil).PruneCold(context.Background(), func(string, time.Time) bool { return true }); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil manager prune = %v, want ErrClosed", err)
	}
	closed := &Manager{cfg: Config{ParquetDir: t.TempDir()}, closed: true}
	if _, err := closed.PruneCold(context.Background(), func(string, time.Time) bool { return true }); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed manager prune = %v, want ErrClosed", err)
	}
}

// The prune must wait for an in-flight export (shared lock) to finish, the
// same exclusion PurgeColdScope relies on.
func TestPruneColdWaitsForAnInFlightExport(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{cfg: Config{ParquetDir: dir}}
	writeColdFile(t, dir, "events", "2000-01-01", "part.parquet", 1)
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = m.holdColdTier(func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	done := make(chan struct{})
	go func() {
		_, _ = m.PruneCold(context.Background(), func(string, time.Time) bool { return true })
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("prune ran while an export held the cold tier")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("prune never ran after the export finished")
	}
}

// A dt= directory swapped for a symlink after it was listed must not redirect
// the delete outside the archive (audit P2-4). The predicate runs between the
// listing and the removal, which is exactly the race window.
func TestPruneColdDoesNotFollowADayDirSwappedForASymlink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "part.parquet")
	if err := os.WriteFile(victim, []byte("not the archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	day := writeColdFile(t, dir, "events", "2020-01-01", "part.parquet", 8)
	m := &Manager{cfg: Config{ParquetDir: dir}}
	_, err := m.PruneCold(context.Background(), func(string, time.Time) bool {
		if err := os.RemoveAll(day); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, day); err != nil {
			t.Fatal(err)
		}
		return true
	})
	if !exists(victim) {
		t.Fatal("prune followed a swapped-in symlink and deleted a file outside the archive")
	}
	if err == nil {
		t.Fatal("a swapped day must be reported, not silently counted")
	}
}

// A table directory that is a symlink out of the archive is refused.
func TestPruneColdRefusesATableDirOutsideTheArchive(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeColdFile(t, outside, "x", "2020-01-01", "part.parquet", 8)
	if err := os.Symlink(filepath.Join(outside, "x"), filepath.Join(dir, "events")); err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: Config{ParquetDir: dir}}
	if _, err := m.PruneCold(context.Background(), func(string, time.Time) bool { return true }); err == nil {
		t.Fatal("prune walked a table directory outside the archive")
	}
	if !exists(filepath.Join(outside, "x", "dt=2020-01-01", "part.parquet")) {
		t.Fatal("file outside the archive deleted")
	}
}

// The erasure purge resolved dt=* by name with filepath.Glob, so a dt= entry
// that is a symlink out of the archive had its part.parquet.tmp deleted
// (fix audit F, the twin P2-4 missed). It now goes through the walker.
func TestPurgeDoesNotFollowADtSymlinkOutOfTheArchive(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "part.parquet.tmp")
	if err := os.WriteFile(victim, []byte("not flare's"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "events"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "events", "dt=2026-01-01")); err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: Config{ParquetDir: dir}}
	_ = m.PurgeColdScope(context.Background(), "org_id", "o1")
	if !exists(victim) {
		t.Fatal("PurgeColdScope deleted a file outside the archive through a dt= symlink")
	}
}

// os.Root refuses an escape from the archive but allows a symlink to ANOTHER
// day inside it. Swapping dt=D for a link to dt=E between the check and the
// open deleted E's archive (fix audit G); the SameFile check refuses it.
func TestPruneRefusesADaySwappedForAnotherDayInsideTheArchive(t *testing.T) {
	dir := t.TempDir()
	victimDay := writeColdFile(t, dir, "events", "2020-01-02", "part.parquet", 8)
	victim := filepath.Join(victimDay, "part.parquet")
	swapped := writeColdFile(t, dir, "events", "2020-01-01", "part.parquet", 8)
	testHookAfterDayLstat = func(_, name string) {
		if name != "dt=2020-01-01" {
			return
		}
		if err := os.RemoveAll(swapped); err != nil {
			t.Fatal(err)
		}
		// Relative, so it stays inside the archive and os.Root follows it.
		if err := os.Symlink(filepath.Base(victimDay), swapped); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { testHookAfterDayLstat = nil }()
	m := &Manager{cfg: Config{ParquetDir: dir}}
	// Only 2020-01-01 is expired; 2020-01-02 must survive.
	_, err := m.PruneCold(context.Background(), func(_ string, d time.Time) bool { return d.Format("2006-01-02") == "2020-01-01" })
	if !exists(victim) {
		t.Fatal("prune followed an intra-archive swap and deleted another day's archive")
	}
	if err == nil {
		t.Fatal("a swapped day must be reported")
	}
}
