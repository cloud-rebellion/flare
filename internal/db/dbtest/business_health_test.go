package dbtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

func businessTestHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// This test is skipped without TEST_DATABASE_URL. With Postgres it proves the
// business aggregate carries both project and organization predicates and that
// the receipt foreign key can support one durable acceptance transaction.
func TestBusinessHealthQueriesAreTenantScoped(t *testing.T) {
	ctx := context.Background()
	_, q := newPool(t)
	orgA, projectA := seedProject(t, ctx, q)
	orgB, projectB := seedProject(t, ctx, q)
	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	newKey := func(org, project string) string {
		t.Helper()
		keyID := id.New()
		_, err := q.CreateBusinessCheckKey(ctx, generated.CreateBusinessCheckKeyParams{
			ID: keyID, OrgID: org, ProjectID: project, Name: "test backend",
			KeyHash: businessTestHash(id.New()), KeyPrefix: "flb_00000000",
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		})
		if err != nil {
			t.Fatalf("create business key: %v", err)
		}
		return keyID
	}
	keyA := newKey(orgA, projectA)
	keyB := newKey(orgB, projectB)
	insert := func(org, project, name, outcome string) {
		t.Helper()
		receiptID := id.New()
		if _, err := q.InsertIngestReceipt(ctx, generated.InsertIngestReceiptParams{
			ID: receiptID, OrgID: org, ProjectID: project, Pillar: "business", Protocol: "native",
			ReceivedCount: 1, AcceptedCount: 1, PersistedCount: 1, Outcome: "accepted", HttpStatus: 202,
			Retryable: false, ReceivedAt: now, CompletedAt: now,
		}); err != nil {
			t.Fatalf("insert receipt: %v", err)
		}
		_, err := q.InsertBusinessCheck(ctx, generated.InsertBusinessCheckParams{
			ID: id.New(), OrgID: org, ProjectID: project, ReceiptID: receiptID, CheckName: name,
			BusinessKeyID:      map[string]string{projectA: keyA, projectB: keyB}[project],
			IdempotencyKeyHash: businessTestHash(receiptID), PayloadHash: businessTestHash(name + outcome),
			Outcome: outcome, StatusClass: map[string]string{"ok": "2xx", "error": "5xx"}[outcome],
			ErrorCode: map[string]string{"ok": "", "error": "provider_error"}[outcome], ElapsedMs: 42, CompletedAt: now,
		})
		if err != nil {
			t.Fatalf("insert business check: %v", err)
		}
	}
	insert(orgA, projectA, "checkout.completed", "ok")
	insert(orgA, projectA, "checkout.completed", "error")
	insert(orgB, projectB, "checkout.completed", "error")

	rows, err := q.ListBusinessHealthByProject(ctx, generated.ListBusinessHealthByProjectParams{
		ProjectID: projectA, OrgID: orgA, Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}, RowLimit: 100,
	})
	if err != nil {
		t.Fatalf("list business health: %v", err)
	}
	if len(rows) != 1 || rows[0].Attempts != 2 || rows[0].OkCount != 1 || rows[0].ErrorCount != 1 {
		t.Fatalf("project A aggregate = %#v, want 2 attempts / 1 ok / 1 error", rows)
	}
	other, err := q.ListBusinessHealthByProject(ctx, generated.ListBusinessHealthByProjectParams{
		ProjectID: projectA, OrgID: orgB, Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}, RowLimit: 100,
	})
	if err != nil {
		t.Fatalf("cross-tenant list: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("cross-tenant aggregate returned %#v", other)
	}

	// A raw writer must not attach an accepted receipt from another project
	// or organization to a business result. Both foreign keys must agree with
	// the tenant scope of the receipt, independently of the API handler.
	receiptID := id.New()
	if _, err := q.InsertIngestReceipt(ctx, generated.InsertIngestReceiptParams{
		ID: receiptID, OrgID: orgA, ProjectID: projectA, Pillar: "business", Protocol: "native",
		ReceivedCount: 1, AcceptedCount: 1, PersistedCount: 1, Outcome: "accepted", HttpStatus: 202,
		Retryable: false, ReceivedAt: now, CompletedAt: now,
	}); err != nil {
		t.Fatalf("insert unlinked receipt: %v", err)
	}
	secondProject, err := q.CreateProject(ctx, generated.CreateProjectParams{
		ID: id.New(), OrgID: orgA, Name: "same-org", Slug: id.New(), Platform: "go",
		PublicKey: id.New(), DsnID: id.New()[:12],
	})
	if err != nil {
		t.Fatalf("create second project: %v", err)
	}
	secondKey := newKey(orgA, secondProject.ID)
	for _, tc := range []struct {
		name, org, project, businessKeyID string
	}{
		{"other project in same organization", orgA, secondProject.ID, secondKey},
		{"other organization and project", orgB, projectB, keyB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := q.InsertBusinessCheck(ctx, generated.InsertBusinessCheckParams{
				ID: id.New(), OrgID: tc.org, ProjectID: tc.project, ReceiptID: receiptID,
				BusinessKeyID:      tc.businessKeyID,
				IdempotencyKeyHash: businessTestHash(id.New()), PayloadHash: businessTestHash("payload"),
				CheckName: "checkout.completed", Outcome: "ok", StatusClass: "2xx",
				ElapsedMs: 42, CompletedAt: now,
			})
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23503" || pgErr.ConstraintName != "business_checks_receipt_scope_fk" {
				t.Fatalf("cross-scope receipt link error = %v, want business_checks_receipt_scope_fk", err)
			}
		})
	}
}
