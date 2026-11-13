package db

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/bright-interaction/flare/internal/id"
)

// Migration 093 flags the operator workspace on an upgraded install: the
// oldest org, with id breaking a created_at tie (the old "first org" query
// had no tiebreak, so the answer depended on the plan).
func TestMigration093FlagsTheOldestOrgWithATiebreak(t *testing.T) {
	adminURL := os.Getenv("FLARE_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("FLARE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	url, sqlDB := migrationTestDB(t, adminURL)
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, sqlDB, "migrations", 92); err != nil {
		t.Fatal(err)
	}
	for _, o := range []struct{ id, at string }{
		{"org_c", "2026-01-02T00:00:00Z"},
		{"org_b", "2026-01-01T00:00:00Z"},
		{"org_a", "2026-01-01T00:00:00Z"},
	} {
		if _, err := sqlDB.ExecContext(ctx, `INSERT INTO orgs (id, name, slug, created_at) VALUES ($1, $1, $1, $2)`, o.id, o.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := RunMigrations(ctx, url); err != nil {
		t.Fatal(err)
	}
	rows, err := sqlDB.QueryContext(ctx, `SELECT id FROM orgs WHERE is_operator ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v string
		_ = rows.Scan(&v)
		got = append(got, v)
	}
	if len(got) != 1 || got[0] != "org_a" {
		t.Fatalf("operator after migration = %v, want [org_a]", got)
	}
	if err := goose.DownToContext(ctx, sqlDB, "migrations", 92); err != nil {
		t.Fatalf("093 down: %v", err)
	}
	if err := RunMigrations(ctx, url); err != nil {
		t.Fatalf("093 up again: %v", err)
	}
}

// Migration 093 must not queue indefinitely behind an open reader of orgs
// (fix audit I): with lock_timeout it fails fast and the deploy retries.
func TestMigration093GivesUpBehindAnOpenReader(t *testing.T) {
	adminURL := os.Getenv("FLARE_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("FLARE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	url, sqlDB := migrationTestDB(t, adminURL)
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, sqlDB, "migrations", 92); err != nil {
		t.Fatal(err)
	}
	reader, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback() }()
	if _, err := reader.ExecContext(ctx, `SELECT count(*) FROM orgs`); err != nil {
		t.Fatal(err)
	}
	mctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	started := time.Now()
	err = RunMigrations(mctx, url)
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "lock timeout") || elapsed > 10*time.Second {
		t.Fatalf("093 behind an open reader: err=%v after %s; want a lock timeout within 10s", err, elapsed.Round(time.Millisecond))
	}
}

// migrationTestDB creates a fresh database and drops it afterwards. The admin
// pool used for the DROP is opened inside the cleanup, so it is still alive
// when the cleanup runs (a deferred Close used to run first and leak it).
func migrationTestDB(t *testing.T, adminURL string) (string, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	name := "flare_test_" + strings.ToLower(id.New())
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	admin.Close()
	if err != nil {
		t.Fatal(err)
	}
	url := adminURL[:strings.LastIndex(adminURL, "/")+1] + name
	if q := strings.IndexByte(adminURL, '?'); q >= 0 {
		url = adminURL[:strings.LastIndex(adminURL[:q], "/")+1] + name + adminURL[q:]
	}
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
		cleanup, err := pgxpool.New(context.Background(), adminURL)
		if err != nil {
			t.Logf("drop %s: %v", name, err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	return url, sqlDB
}
