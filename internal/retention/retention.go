// Package retention decides how long telemetry lives in each storage tier.
//
// Two tiers, one policy:
//
//   - hot: the daily Postgres partitions of events/logs/spans/metrics. A day
//     older than HotDays is exported (unless archiving is off) and dropped.
//   - cold: the Parquet archive those exports land in. Mode "forever" keeps it
//     forever (the behaviour before this package existed), "days" prunes
//     archive days older than ColdDays, "off" stops archiving altogether and
//     drops hot partitions without exporting them.
//
// The policy is instance-wide because the partitions and the archive are
// shared by every org. It is stored in retention_settings when an admin saves
// it in the dashboard; with nothing stored, the environment (RETENTION_DAYS,
// COLD_RETENTION_DAYS) is the effective policy, unchanged from before.
//
// Everything here is pure (no I/O) so the boundary rules are table-testable.
package retention

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ColdMode is what happens to a hot partition once it ages out.
type ColdMode string

const (
	ColdForever ColdMode = "forever"
	ColdDays    ColdMode = "days"
	ColdOff     ColdMode = "off"
)

// Bounds on a policy an admin may store.
//
// MinHotDays is 7 because ingest accepts client timestamps up to
// ingest.MaxBackfillDays (7) old and the partition manager pre-creates that
// whole backfill window on every pass. A shorter hot window would recreate a
// partition, let ingest write into it, then drop it on the same pass; with an
// export already recorded for that day the late rows would vanish unarchived.
// (Not imported from ingest, for the same layering reason as partition's
// behindDays; partition's tests pin the relationship.)
const (
	MinHotDays  = 7
	MaxHotDays  = 365
	MaxColdDays = 3650
)

// Policy is one effective retention configuration.
type Policy struct {
	HotDays  int      `json:"hot_retention_days"`
	ColdMode ColdMode `json:"cold_mode"`
	// ColdDays is meaningful only when ColdMode == ColdDays.
	ColdDays int `json:"cold_retention_days,omitempty"`
}

// Source says where an effective policy came from.
type Source string

const (
	SourceStored Source = "stored"
	SourceEnv    Source = "env"
)

// Archives reports whether aged hot partitions are exported before the drop.
func (p Policy) Archives() bool { return p.ColdMode != ColdOff }

// Validate checks a policy an admin wants to store. Env defaults are not run
// through this: an existing install with RETENTION_DAYS=3 keeps working as it
// did, and the stricter bounds only apply to values chosen in the UI.
func (p Policy) Validate() error {
	if p.HotDays < MinHotDays || p.HotDays > MaxHotDays {
		return fmt.Errorf("hot retention must be between %d and %d days", MinHotDays, MaxHotDays)
	}
	switch p.ColdMode {
	case ColdForever, ColdOff:
		if p.ColdDays != 0 {
			return fmt.Errorf("cold retention days only apply when cold mode is %q", ColdDays)
		}
	case ColdDays:
		if p.ColdDays < p.HotDays || p.ColdDays > MaxColdDays {
			return fmt.Errorf("archive retention must be between the hot retention (%d) and %d days", p.HotDays, MaxColdDays)
		}
	default:
		return fmt.Errorf("cold mode must be one of forever, days, off")
	}
	return nil
}

// Resolve picks the effective policy: a stored policy wins when present, the
// environment default otherwise. A stored policy that fails validation is an
// error, never a silent fallback to env: the env value may be SHORTER than
// what the admin stored, and falling back would delete data they kept.
func Resolve(stored *Policy, env Policy) (Policy, Source, error) {
	if stored == nil {
		return env, SourceEnv, nil
	}
	if err := stored.Validate(); err != nil {
		return Policy{}, "", fmt.Errorf("stored retention policy is invalid: %w", err)
	}
	return *stored, SourceStored, nil
}

// ParseColdEnv parses COLD_RETENTION_DAYS. Empty or "forever" keeps the
// archive forever (the historical behaviour). A positive integer prunes the
// archive after that many days. Anything else, including 0 and "off", is an
// error: turning archiving off drops data without exporting it and is only
// accepted as an explicit choice in the dashboard.
func ParseColdEnv(v string) (ColdMode, int, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" || v == "forever" {
		return ColdForever, 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > MaxColdDays {
		return "", 0, fmt.Errorf("COLD_RETENTION_DAYS must be empty, \"forever\" or 1..%d (got %q)", MaxColdDays, v)
	}
	return ColdDays, n, nil
}

// Today is the UTC calendar day containing now, at 00:00.
func Today(now time.Time) time.Time { return now.UTC().Truncate(24 * time.Hour) }

// Cutoff is the first day that is KEPT for a window of days: every day
// strictly before it is past retention.
func Cutoff(now time.Time, days int) time.Time {
	return Today(now).AddDate(0, 0, -days)
}

// ShouldDropHot reports whether the hot partition for day is past retention.
// Today and future days are never dropped, whatever hotDays says: hotDays < 1
// is clamped, and the today check stands on its own so a clock or arithmetic
// mistake upstream cannot reach the partition ingest is writing to.
func ShouldDropHot(now, day time.Time, hotDays int) bool {
	if hotDays < 1 {
		hotDays = 1
	}
	day = day.UTC().Truncate(24 * time.Hour)
	if !day.Before(Today(now)) {
		return false
	}
	return day.Before(Cutoff(now, hotDays))
}

// ShouldPruneCold reports whether the archived day is past cold retention.
// Only ColdDays prunes. The archive is never pruned inside the hot window
// (MinHotDays), so a mis-set policy cannot delete the archive of a day whose
// hot partition still exists.
func ShouldPruneCold(now, day time.Time, p Policy) bool {
	if p.ColdMode != ColdDays {
		return false
	}
	days := p.ColdDays
	if days < p.HotDays {
		days = p.HotDays
	}
	if days < MinHotDays {
		days = MinHotDays
	}
	day = day.UTC().Truncate(24 * time.Hour)
	return day.Before(Cutoff(now, days))
}

// ConservativeForArchivePrune returns, of two policies read at different
// moments, the one that prunes less of the archive: any mode other than "days"
// prunes nothing and wins outright; between two day counts, the longer archive
// window and the longer hot window (ShouldPruneCold clamps to both) win.
func ConservativeForArchivePrune(a, b Policy) Policy {
	if a.ColdMode != ColdDays {
		return a
	}
	if b.ColdMode != ColdDays {
		return b
	}
	return Policy{HotDays: max(a.HotDays, b.HotDays), ColdMode: ColdDays, ColdDays: max(a.ColdDays, b.ColdDays)}
}

// Reduces reports whether moving from cur to next keeps LESS data in either
// tier, which is what requires an explicit confirmation from the admin.
//
// The archive is ranked by what is on disk, not by the mode's name. "off"
// stops new exports but never prunes, so every day archived before it was
// chosen is still there. Moving INTO "days" from any other mode therefore
// prunes data the previous mode kept, whatever the day count, and so does
// lowering the count within "days". Moving into "off" from anything else
// stops archiving the days that age out next, which drops them unexported.
func Reduces(cur, next Policy) bool {
	if next.HotDays < cur.HotDays {
		return true
	}
	switch next.ColdMode {
	case ColdOff:
		return cur.ColdMode != ColdOff
	case ColdDays:
		return cur.ColdMode != ColdDays || next.ColdDays < cur.ColdDays
	default:
		return false
	}
}

// ErrReductionNotConfirmed is returned when a change would remove data and the
// caller did not confirm it.
var ErrReductionNotConfirmed = errors.New("this change removes stored telemetry; confirm it to proceed")

// Describe renders a policy for audit rows and logs.
func (p Policy) Describe() string {
	switch p.ColdMode {
	case ColdDays:
		return fmt.Sprintf("hot %dd, archive %dd", p.HotDays, p.ColdDays)
	case ColdOff:
		return fmt.Sprintf("hot %dd, no archive", p.HotDays)
	default:
		return fmt.Sprintf("hot %dd, archive forever", p.HotDays)
	}
}
