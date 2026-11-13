package analytics

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// The Parquet archive walker. Every piece of this package that enumerates
// archive days or deletes anything inside the archive goes through here:
// cold pruning, the erasure purge and the inventory. The walk is
// fd-relative: the archive, each table and each day are opened as os.Root,
// so a name is never re-resolved between the check and the delete.
//
// internal/ciguard fails the build on filepath.Glob, os.Remove or
// os.RemoveAll anywhere else in internal/analytics. Two copies of this logic
// are how the P2-4 symlink fix reached prune and missed purge.

// The export layout is <dir>/<table>/dt=YYYY-MM-DD/part.parquet. Only names
// matching it exactly are ever considered.
var dtDirRe = regexp.MustCompile(`^dt=([0-9]{4}-[0-9]{2}-[0-9]{2})$`)

// errDaySwapped is returned when a day directory is not the directory that
// was checked: it became a symlink, or something else was put in its place.
var errDaySwapped = errors.New("day directory changed while it was being opened, leaving it in place")

// testHookAfterDayLstat runs between the Lstat of a day and opening it, so a
// test can swap the directory in exactly that window. Nil outside tests.
var testHookAfterDayLstat func(table, name string)

type archiveDayName struct {
	name string
	day  time.Time
}

// archiveDay is one opened dt= directory.
type archiveDay struct {
	Table string
	Name  string
	Day   time.Time
	// Path is for DuckDB, which can only open files by path. It names the
	// directory that was verified when the day was opened.
	Path   string
	dir    *os.Root
	parent *os.Root
}

// walkArchiveDays calls visit for every dt=YYYY-MM-DD directory of every
// cold table, oldest table order first. want, when not nil, filters days
// before they are opened. A missing archive or table is empty. A table
// directory that resolves outside the archive, or one that cannot be listed,
// stops the walk with an error. A day that cannot be opened safely is
// reported through skip and the walk continues. visit returning an error
// stops the walk.
func walkArchiveDays(archive string, tables []string, want func(table string, day time.Time) bool,
	visit func(d *archiveDay) error, skip func(table, name string, err error)) error {
	root, err := os.OpenRoot(archive)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open cold tier: %w", err)
	}
	defer root.Close()
	for _, table := range tables {
		if err := walkTable(root, archive, table, want, visit, skip); err != nil {
			return err
		}
	}
	return nil
}

func walkTable(root *os.Root, archive, table string, want func(string, time.Time) bool,
	visit func(*archiveDay) error, skip func(string, string, error)) error {
	tr, err := root.OpenRoot(table)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open cold tier for %s: %w", table, err)
	}
	defer tr.Close()
	days, err := listDays(tr)
	if err != nil {
		return fmt.Errorf("list cold tier for %s: %w", table, err)
	}
	for _, d := range days {
		if want != nil && !want(table, d.day) {
			continue
		}
		dr, err := openDay(tr, table, d.name)
		if err != nil {
			if skip != nil {
				skip(table, d.name, err)
			}
			continue
		}
		err = visit(&archiveDay{Table: table, Name: d.name, Day: d.day, Path: filepath.Join(archive, table, d.name), dir: dr, parent: tr})
		_ = dr.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// listDays returns the dt=YYYY-MM-DD directories of one table root. A symlink
// is never a day: it reports its own type, not a directory.
func listDays(tr *os.Root) ([]archiveDayName, error) {
	f, err := tr.Open(".")
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	var out []archiveDayName
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m := dtDirRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		day, err := time.Parse("2006-01-02", m[1])
		if err != nil {
			continue
		}
		out = append(out, archiveDayName{name: e.Name(), day: day})
	}
	return out, nil
}

// openDay opens one day as a nested os.Root, but only if it is the real
// directory that Lstat saw. os.Root already refuses a symlink that leaves the
// archive; the SameFile check also refuses one that points at ANOTHER day
// inside it, which os.Root allows.
func openDay(tr *os.Root, table, name string) (*os.Root, error) {
	before, err := tr.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, errDaySwapped
	}
	if testHookAfterDayLstat != nil {
		testHookAfterDayLstat(table, name)
	}
	dr, err := tr.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := dr.Stat(".")
	if err != nil || !os.SameFile(before, opened) {
		_ = dr.Close()
		if err != nil {
			return nil, err
		}
		return nil, errDaySwapped
	}
	return dr, nil
}

// entries lists the day's directory entries.
func (d *archiveDay) entries() ([]fs.DirEntry, error) {
	f, err := d.dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// has reports whether the day holds a regular file called name.
func (d *archiveDay) has(name string) bool {
	info, err := d.dir.Lstat(name)
	return err == nil && info.Mode().IsRegular()
}

// remove deletes one file of the day, relative to the verified directory.
func (d *archiveDay) remove(name string) error { return d.dir.Remove(name) }

// rename replaces one file of the day with another, relative to it.
func (d *archiveDay) rename(from, to string) error { return d.dir.Rename(from, to) }

// removeSelf removes the emptied day directory from its table root. It fails,
// harmlessly, while the directory still holds anything. Remove never follows
// a final symlink, so a swap here removes only the link.
func (d *archiveDay) removeSelf() error { return d.parent.Remove(d.Name) }

// archiveHasDay reports whether any part.parquet exists for the table, for
// the query planner. Read-only, and it follows a symlinked table directory
// on purpose: so does the read_parquet glob the query then runs.
func archiveHasDay(archive, table string) bool {
	entries, err := os.ReadDir(filepath.Join(archive, table))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() || !dtDirRe.MatchString(e.Name()) {
			continue
		}
		if info, err := os.Stat(filepath.Join(archive, table, e.Name(), "part.parquet")); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}
