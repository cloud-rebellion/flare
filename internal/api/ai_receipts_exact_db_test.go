package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// Exercise the registered route, real authentication and PostgreSQL, including
// a run that has fallen outside the largest supported recent-receipts list.
func TestGetAIActionReceiptExactScopeAndLifecycle(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	q := generated.New(pool)
	project := seedProjectForIngest(t, pool)
	foreignProject := seedProjectForIngest(t, pool)
	siblingProject, err := q.CreateProject(ctx, generated.CreateProjectParams{
		ID: id.New(), OrgID: project.OrgID, Name: "sibling", Slug: "sibling-" + id.New(),
		Platform: "go", PublicKey: id.New(), DsnID: id.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	issue, err := q.UpsertIssue(ctx, generated.UpsertIssueParams{
		ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID,
		Fingerprint: id.New(), Title: "exact receipt fixture", Culprit: "test", Level: "error", Platform: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	target, err := q.InsertAIActionReceipt(ctx, generated.InsertAIActionReceiptParams{
		ID: id.New(), OrgID: project.OrgID, ProjectID: project.ID, IssueID: issue.ID,
		Operation: "triage", Status: "started", Provider: "unknown", Retryable: true,
		StartedAt: pgtype.Timestamptz{Time: started, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// One bounded SQL statement seeds 501 newer runs; no provider call or raw
	// prompt/completion fixture is involved.
	if _, err := pool.Exec(ctx, `INSERT INTO ai_action_receipts
		(id,org_id,project_id,issue_id,operation,status,provider,started_at)
		SELECT $1 || '-' || n,$2,$3,$4,'triage','cached','cache',now()
		FROM generate_series(1,501) AS n`, id.New(), project.OrgID, project.ID, issue.ID); err != nil {
		t.Fatal(err)
	}
	mintViewer := func(org string) string {
		t.Helper()
		token := "synthetic-viewer-" + id.New()
		if _, err := q.CreateAPIKey(ctx, generated.CreateAPIKeyParams{
			ID: id.New(), OrgID: org, Name: "exact receipt viewer",
			KeyHash: auth.HashAPIKey(token), KeyPrefix: "synthetic", Role: "viewer",
		}); err != nil {
			t.Fatal(err)
		}
		return token
	}
	viewer, foreignViewer := mintViewer(project.OrgID), mintViewer(foreignProject.OrgID)
	s := serverForDB(pool)
	s.sessions = auth.NewSessionManager(time.Hour, time.Hour, false)
	handler := s.Routes(fstest.MapFS{"index.html": {Data: []byte("test")}}, func(next http.Handler) http.Handler { return next })
	request := func(projectID, receiptID, token string) *httptest.ResponseRecorder {
		t.Helper()
		path := "/api/projects/" + projectID + "/ai-receipts"
		if receiptID != "" {
			path += "/" + receiptID
		} else {
			path += "?limit=500"
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	list := request(project.ID, "", viewer)
	var recent []aiActionReceiptResponse
	if err := json.Unmarshal(list.Body.Bytes(), &recent); err != nil || list.Code != http.StatusOK || len(recent) != 500 {
		t.Fatalf("recent list = %d %s: %v", list.Code, list.Body.String(), err)
	}
	for _, row := range recent {
		if row.ID == target.ID {
			t.Fatal("old target unexpectedly remained in the newest 500 receipts")
		}
	}
	read := request(project.ID, target.ID, viewer)
	var initial aiActionReceiptResponse
	if err := json.Unmarshal(read.Body.Bytes(), &initial); err != nil || read.Code != http.StatusOK {
		t.Fatalf("exact started receipt = %d %s: %v", read.Code, read.Body.String(), err)
	}
	if initial.ID != target.ID || initial.IssueID != issue.ID || initial.Operation != "triage" || initial.Status != "started" ||
		initial.Provider != "unknown" || !initial.Retryable || initial.CompletedAt != nil || initial.EvidenceDigest != "" ||
		initial.StartedAt == nil || !initial.StartedAt.Equal(started) || initial.CreatedAt == nil {
		t.Fatalf("started representation = %#v", initial)
	}
	if strings.Contains(read.Body.String(), "completed_at") || strings.Contains(read.Body.String(), "evidence_digest") {
		t.Fatal("unfinished run exposed completion or fresh-evidence fields")
	}
	for name, tc := range map[string]struct{ projectID, receiptID, token string }{
		"wrong organization": {project.ID, target.ID, foreignViewer},
		"wrong project":      {siblingProject.ID, target.ID, viewer},
		"foreign project":    {foreignProject.ID, target.ID, viewer},
		"missing project":    {id.New(), target.ID, viewer},
		"missing receipt":    {project.ID, id.New(), viewer},
	} {
		t.Run(name, func(t *testing.T) {
			rec := request(tc.projectID, tc.receiptID, tc.token)
			if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), target.ID) || strings.Contains(rec.Body.String(), issue.ID) {
				t.Fatalf("scope refusal = %d %s", rec.Code, rec.Body.String())
			}
		})
	}
	if unauthenticated := request(project.ID, target.ID, ""); unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated exact read = %d %s", unauthenticated.Code, unauthenticated.Body.String())
	}
	// Also pin the SQL scope independently of the handler's project check.
	if _, err := q.GetAIActionReceiptByProject(ctx, generated.GetAIActionReceiptByProjectParams{
		OrgID: foreignProject.OrgID, ProjectID: project.ID, ID: target.ID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-tenant exact SQL read = %v", err)
	}
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	completed := started.Add(time.Second)
	if _, err := q.CompleteAIActionReceipt(ctx, generated.CompleteAIActionReceiptParams{
		ID: target.ID, OrgID: project.OrgID, ProjectID: project.ID,
		Status: "succeeded", Provider: "openai", EvidenceDigest: digest,
		CompletedAt: pgtype.Timestamptz{Time: completed, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	read = request(project.ID, target.ID, viewer)
	var succeeded aiActionReceiptResponse
	if err := json.Unmarshal(read.Body.Bytes(), &succeeded); err != nil || read.Code != http.StatusOK {
		t.Fatalf("exact succeeded receipt = %d %s: %v", read.Code, read.Body.String(), err)
	}
	if succeeded.ID != target.ID || succeeded.IssueID != issue.ID || succeeded.Operation != "triage" ||
		succeeded.Status != "succeeded" || succeeded.Provider != "openai" || succeeded.Retryable || succeeded.EvidenceDigest != digest ||
		succeeded.CompletedAt == nil || !succeeded.CompletedAt.Equal(completed) || succeeded.ErrorCode != "" {
		t.Fatalf("succeeded representation = %#v", succeeded)
	}
	for _, forbidden := range []string{"org_id", "project_id", "prompt", "completion", "credential"} {
		if strings.Contains(read.Body.String(), forbidden) {
			t.Fatalf("exact receipt response exposed %q", forbidden)
		}
	}
}
