package analytics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bright-interaction/flare/internal/partition"
)

// prunableFiles are the only file names cold pruning deletes inside a day
// directory: the export itself, and the temporaries an interrupted export
// (.tmp) or erasure rewrite (.purge) can leave behind. A day directory holding
// anything else is skipped and reported, never removed wholesale: the
// directory may be shared with something Flare did not write.
var prunableFiles = map[string]bool{
	"part.parquet":       true,
	"part.parquet.tmp":   true,
	"part.parquet.purge": true,
}

// ColdDayUsage is the on-disk size of one archived day of one table.
type ColdDayUsage struct {
	Table string    `json:"table"`
	Day   time.Time `json:"day"`
	Bytes int64     `json:"bytes"`
	Files int       `json:"files"`
}

// ColdPruneSupported reports whether this archive can be pruned by Flare.
// S3/MinIO cannot: DuckDB's httpfs has no object delete, the same reason
// PurgeColdScope refuses there.
func (m *Manager) ColdPruneSupported() bool { return m != nil && m.cfg.S3Endpoint == "" }

// ColdKind is "local" or "s3".
func (m *Manager) ColdKind() string {
	if m != nil && m.cfg.S3Endpoint != "" {
		return "s3"
	}
	return "local"
}

// PruneCold deletes every archived day for which expired returns true, from
// the local Parquet cold tier.
//
// It takes the EXCLUSIVE analytics lock, like PurgeColdScope: no export can be
// mid-write and no erasure rewrite can be running while files disappear, and no
// analytics read sees a half-deleted day. Deleting only ever removes data, so
// it cannot undo a right-to-erasure purge, and pending_erasures bookkeeping is
// left exactly as it is (an S3 obligation stays open until a human closes it).
//
// Fail closed: an unreadable table directory (or one that is a symlink out of
// the archive) stops the prune with an error; a day directory with unexpected
// contents, or one replaced by a symlink after listing, is skipped and
// reported. What was
// already deleted before the error is returned in the result.
func (m *Manager) PruneCold(ctx context.Context, expired func(table string, day time.Time) bool) (partition.ColdPruneResult, error) {
	var res partition.ColdPruneResult
	if m == nil {
		return res, ErrClosed
	}
	if m.cfg.S3Endpoint != "" {
		return res, partition.ErrColdPruneUnsupported
	}
	if expired == nil {
		return res, fmt.Errorf("analytics: cold prune needs an expiry predicate")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return res, ErrClosed
	}
	// The shared archive walker opens the archive, each table and each day
	// fd-relative and verifies each day is the directory it listed, so a dt=
	// directory swapped for a symlink cannot redirect a delete.
	var skipped []error
	err := walkArchiveDays(m.cfg.ParquetDir, coldTables, expired, func(d *archiveDay) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		files, bytes, err := removeArchiveDay(d)
		res.Files += files
		res.Bytes += bytes
		if err != nil {
			skipped = append(skipped, fmt.Errorf("%s/%s: %w", d.Table, d.Name, err))
			return nil
		}
		res.Days++
		res.Pruned = append(res.Pruned, partition.PrunedDay{Table: d.Table, Day: d.Day})
		return nil
	}, func(table, name string, err error) {
		skipped = append(skipped, fmt.Errorf("%s/%s: %w", table, name, err))
	})
	if err != nil {
		return res, fmt.Errorf("analytics: %w", err)
	}
	return res, errors.Join(skipped...)
}

// removeArchiveDay deletes one archived day: its known files, then the
// directory. Anything unexpected inside means nothing is deleted.
func removeArchiveDay(d *archiveDay) (files int, bytes int64, err error) {
	entries, err := d.entries()
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if !prunableFiles[e.Name()] || !e.Type().IsRegular() {
			return 0, 0, fmt.Errorf("unexpected entry %q, leaving the day in place", e.Name())
		}
	}
	for _, e := range entries {
		info, ierr := e.Info()
		if rerr := d.remove(e.Name()); rerr != nil {
			return files, bytes, rerr
		}
		files++
		if ierr == nil {
			bytes += info.Size()
		}
	}
	return files, bytes, d.removeSelf()
}

// ColdInventory lists the archived days of the local cold tier with their
// size. S3 returns partition.ErrColdPruneUnsupported (Flare cannot list or
// size objects there without the bucket's own tooling).
func (m *Manager) ColdInventory(ctx context.Context) ([]ColdDayUsage, error) {
	if m == nil {
		return nil, ErrClosed
	}
	if m.cfg.S3Endpoint != "" {
		return nil, partition.ErrColdPruneUnsupported
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, ErrClosed
	}
	out := []ColdDayUsage{}
	err := walkArchiveDays(m.cfg.ParquetDir, coldTables, nil, func(d *archiveDay) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		u := ColdDayUsage{Table: d.Table, Day: d.Day}
		entries, err := d.entries()
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			u.Files++
			u.Bytes += info.Size()
		}
		out = append(out, u)
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	return out, nil
}
