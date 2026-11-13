package partition

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DayUsage is the approximate size of one table's partition for one day.
// Rows come from pg_class.reltuples (planner statistics, refreshed by
// autovacuum/ANALYZE), so they are an estimate, which is all a retention
// preview needs; an exact count(*) over every partition would be a full scan
// of the hot tier on each settings page load.
type DayUsage struct {
	Table string    `json:"table"`
	Day   time.Time `json:"day"`
	Rows  int64     `json:"approx_rows"`
	Bytes int64     `json:"bytes"`
}

// HotInventory lists every dated telemetry partition with its estimated row
// count and on-disk size (table + indexes + toast). Global by design: the
// partitions are shared by every org, and only aggregate sizes leave here.
func HotInventory(ctx context.Context, pool *pgxpool.Pool) ([]DayUsage, error) {
	rows, err := pool.Query(ctx, `
		SELECT parent.relname, c.relname,
		       GREATEST(c.reltuples, 0)::bigint,
		       pg_total_relation_size(c.oid)::bigint
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class parent ON parent.oid = i.inhparent
		WHERE parent.relname = ANY($1)
		  AND c.relname ~ ('^' || parent.relname || '_[0-9]{4}_[0-9]{2}_[0-9]{2}$')
		ORDER BY c.relname`, tables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DayUsage{}
	for rows.Next() {
		var parent, name string
		var u DayUsage
		if err := rows.Scan(&parent, &name, &u.Rows, &u.Bytes); err != nil {
			return nil, err
		}
		d, err := time.Parse("2006_01_02", name[len(parent)+1:])
		if err != nil {
			continue
		}
		u.Table, u.Day = parent, d
		out = append(out, u)
	}
	return out, rows.Err()
}
