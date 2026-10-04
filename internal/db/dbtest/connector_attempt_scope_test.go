package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// With TEST_DATABASE_URL, the real migrated schema must reject a connector
// receipt that pairs one tenant's organization with another tenant's project.
// A valid project receipt and an organization-only receipt must still persist,
// and deleting a project must retain its historical receipt under the org.
func TestConnectorAttemptProjectScopeConstraint(t *testing.T) {
	ctx := context.Background()
	pool, q := newPool(t)
	orgA, projectA := seedProject(t, ctx, q)
	_, projectB := seedProject(t, ctx, q)
	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	insert := func(project pgtype.Text) (string, error) {
		t.Helper()
		attemptID := id.New()
		err := q.InsertConnectorAttempt(ctx, generated.InsertConnectorAttemptParams{
			ID: attemptID, OrgID: orgA, ProjectID: project,
			Provider: "github", Operation: "user", Channel: "http", Outcome: "ok",
			StatusClass: "2xx", AttemptedAt: now, CompletedAt: now,
		})
		return attemptID, err
	}

	if _, err := insert(pgtype.Text{String: projectB, Valid: true}); err == nil {
		t.Fatal("cross-tenant project attribution was accepted")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" || pgErr.ConstraintName != "connector_attempts_project_org_fk" {
			t.Fatalf("cross-tenant insert failed for the wrong reason: %v", err)
		}
	}

	projectReceipt, err := insert(pgtype.Text{String: projectA, Valid: true})
	if err != nil {
		t.Fatalf("valid project receipt: %v", err)
	}
	if _, err := insert(pgtype.Text{}); err != nil {
		t.Fatalf("organization-only receipt: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM projects WHERE id = $1 AND org_id = $2`, projectA, orgA); err != nil {
		t.Fatalf("delete project with historical receipt: %v", err)
	}
	var receiptOrg string
	var receiptProject pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT org_id, project_id FROM connector_attempts WHERE id = $1`, projectReceipt).Scan(&receiptOrg, &receiptProject); err != nil {
		t.Fatalf("read retained historical receipt: %v", err)
	}
	if receiptOrg != orgA || receiptProject.Valid {
		t.Fatalf("retained receipt scope = (%q, %#v), want (%q, NULL)", receiptOrg, receiptProject, orgA)
	}
}
