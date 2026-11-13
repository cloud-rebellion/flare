package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/id"
)

// TestExportHealthStatsRecordsBoundedColdTierState exercises the real
// migration and aggregate query when TEST_DATABASE_URL is available. It uses
// baseline deltas because export_log is intentionally a global operational
// ledger and may already contain rows from another worker.
func TestExportHealthStatsRecordsBoundedColdTierState(t *testing.T) {
	ctx := context.Background()
	pool, q := newPool(t)
	tbl := "health_test_" + id.New()
	day := time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour)
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM export_log WHERE tbl=$1`, tbl) }()

	before, err := q.GetExportHealthStats(ctx)
	if err != nil {
		t.Fatalf("baseline export health: %v", err)
	}
	base := pgtype.Timestamptz{Time: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond), Valid: true}
	rows := []struct {
		status string
		code   string
		bytes  int64
		known  bool
	}{
		{status: "succeeded", bytes: 1234, known: true},
		{status: "failed", code: "cold_write"},
		{status: "attempted"},
	}
	for i, row := range rows {
		at := base.Time.Add(time.Duration(i) * time.Minute)
		if _, err := pool.Exec(ctx, `
			INSERT INTO export_log (tbl, day, status, attempted_at, exported_at, row_count, exported_bytes, bytes_known, export_latency_ms, error_code)
			VALUES ($1,$2,$3,$4,$4,1,$5,$6,17,$7)`,
			tbl, day.AddDate(0, 0, i), row.status,
			pgtype.Timestamptz{Time: at, Valid: true}, row.bytes, row.known, row.code); err != nil {
			t.Fatalf("insert export health row %d: %v", i, err)
		}
	}
	after, err := q.GetExportHealthStats(ctx)
	if err != nil {
		t.Fatalf("aggregate export health: %v", err)
	}
	if after.SuccessCount-before.SuccessCount != 1 || after.FailureCount-before.FailureCount != 1 || after.AttemptedCount-before.AttemptedCount != 1 {
		t.Fatalf("status deltas = success %d/failure %d/attempted %d, want 1/1/1 (before=%#v after=%#v)",
			after.SuccessCount-before.SuccessCount, after.FailureCount-before.FailureCount, after.AttemptedCount-before.AttemptedCount, before, after)
	}
	if after.ExportedBytes-before.ExportedBytes != 1234 || after.BytesObservations-before.BytesObservations != 1 {
		t.Fatalf("byte deltas = %d/%d, want 1234/1", after.ExportedBytes-before.ExportedBytes, after.BytesObservations-before.BytesObservations)
	}
	if after.LatestStatus != "attempted" || after.LatestErrorCode != "cold_write" {
		t.Fatalf("latest state = %q/%q, want attempted/cold_write", after.LatestStatus, after.LatestErrorCode)
	}
	if after.LatestAttemptAt.Time.Before(base.Time.Add(2 * time.Minute)) {
		t.Fatalf("latest attempt timestamp = %v, want at or after %v", after.LatestAttemptAt.Time, base.Time.Add(2*time.Minute))
	}
}
