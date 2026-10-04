package partition

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db"
	"github.com/bright-interaction/flare/internal/id"
)

func TestSentryReplayLedgerPrunesOldRowsInBatches(t *testing.T) {
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
	org, project := id.New(), id.New()
	if _, err := pool.Exec(ctx, `INSERT INTO orgs (id,name,slug) VALUES ($1,'dedup','dedup')`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id,org_id,name,slug,platform,public_key,dsn_id)
		VALUES ($1,$2,'dedup','dedup','other',$3,$4)`, project, org, id.New(), id.New()); err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -40)
	fresh := time.Now()
	for i, at := range []time.Time{old, old, old, fresh} {
		if _, err := pool.Exec(ctx, `INSERT INTO sentry_item_acceptances
			(org_id,project_id,item_type,item_id,payload_sha256,accepted_at)
			VALUES ($1,$2,'event',$3,$4,$5)`, org, project, id.New(), strings.Repeat(string(rune('a'+i)), 64), at); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().AddDate(0, 0, -30)
	first, err := pruneSentryItemAcceptancesBatch(ctx, pool, cutoff, 2)
	if err != nil || first != 2 {
		t.Fatalf("first prune = %d, %v; want 2", first, err)
	}
	second, err := pruneSentryItemAcceptancesBatch(ctx, pool, cutoff, 2)
	if err != nil || second != 1 {
		t.Fatalf("second prune = %d, %v; want 1", second, err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sentry_item_acceptances WHERE org_id=$1 AND project_id=$2`, org, project).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("fresh ledger rows remaining = %d, want 1", remaining)
	}
}
