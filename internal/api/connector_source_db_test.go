package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestConnectorSourceSeparatesPublicReportsFromServerObservations(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	ctx := context.Background()
	connectionID := id.New()
	_, err := s.q.CreateConnectorConnection(ctx, generated.CreateConnectorConnectionParams{
		ID: connectionID, OrgID: project.OrgID, Provider: "hubspot", InstallationID: "hubspot-test",
		InstallationName: "Test", Status: "active", Scopes: json.RawMessage(`[]`), SecretCiphertext: "encrypted-test-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO connector_attempts
        (id, org_id, project_id, connection_id, source_kind, provider, operation, channel, outcome, status_class, elapsed_ms, attempted_at, completed_at)
        VALUES ($1,$2,$3,$4,'client_reported','hubspot','account_details','http','ok','2xx',0,now(),now())`,
		id.New(), project.OrgID, project.ID, connectionID); err == nil {
		t.Fatal("database accepted a client report bound to a provider connection")
	}

	send := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/"+project.DsnID+"/connector-attempts", strings.NewReader(body))
		req.Header.Set("X-Flare-Key", project.PublicKey)
		route := chi.NewRouteContext()
		route.URLParams.Add("projectID", project.DsnID)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
		rec := httptest.NewRecorder()
		s.handleConnectorAttempt(rec, req)
		return rec
	}
	if rec := send(`{"provider":"hubspot","connection_id":"` + connectionID + `","operation":"account_details","channel":"http","outcome":"ok","elapsed_ms":5}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("public DSN impersonated registered connection: %d %s", rec.Code, rec.Body.String())
	}
	if rec := send(`{"provider":"hubspot","operation":"account_details","channel":"http","outcome":"error","status_class":"5xx","elapsed_ms":5,"quota_remaining":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("client observation rejected: %d %s", rec.Code, rec.Body.String())
	}
	now := time.Now().UTC()
	quota := int64(100)
	if err := s.recordConnectorAttempt(ctx, integration.Attempt{
		Scope:    integration.Scope{OrgID: project.OrgID, ProjectID: project.ID, ConnectionID: connectionID},
		Provider: "hubspot", Operation: "account_details", Channel: "http", Outcome: "ok", StatusClass: "2xx",
		QuotaRemaining: &quota, AttemptedAt: now, CompletedAt: now,
	}); err != nil {
		t.Fatalf("server observation: %v", err)
	}

	var clientCount, serverCount, clientBoundCount int
	if err := pool.QueryRow(ctx, `SELECT
        count(*) FILTER (WHERE source_kind='client_reported'),
        count(*) FILTER (WHERE source_kind='server_observed'),
        count(*) FILTER (WHERE source_kind='client_reported' AND connection_id IS NOT NULL)
        FROM connector_attempts WHERE org_id=$1 AND project_id=$2`, project.OrgID, project.ID).
		Scan(&clientCount, &serverCount, &clientBoundCount); err != nil {
		t.Fatal(err)
	}
	if clientCount != 1 || serverCount != 1 || clientBoundCount != 0 {
		t.Fatalf("source counts client/server/bound = %d/%d/%d", clientCount, serverCount, clientBoundCount)
	}

	since := pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}
	for _, tc := range []struct {
		source     string
		wantErrors int64
		wantQuota  int64
		wantConnID string
	}{
		{"client_reported", 1, 1, ""},
		{"server_observed", 0, 100, connectionID},
	} {
		stats, err := s.q.GetConnectorErrorRateStats(ctx, generated.GetConnectorErrorRateStatsParams{
			ProjectID: project.ID, OrgID: project.OrgID, SourceKind: tc.source,
			Provider: "hubspot", Operation: "account_details", Channel: "http", CompletedAt: since,
		})
		if err != nil || stats.Attempts != 1 || stats.ErrorCount != tc.wantErrors {
			t.Fatalf("%s error rate = %#v (%v)", tc.source, stats, err)
		}
		quotaStats, err := s.q.GetConnectorQuotaStats(ctx, generated.GetConnectorQuotaStatsParams{
			ProjectID: project.ID, OrgID: project.OrgID, SourceKind: tc.source,
			Provider: "hubspot", Operation: "account_details", Channel: "http", Metric: "quota_remaining", CompletedAt: since,
		})
		if err != nil || quotaStats.Points != 1 || quotaStats.LatestValue != tc.wantQuota {
			t.Fatalf("%s quota = %#v (%v)", tc.source, quotaStats, err)
		}
		rows, err := s.q.ListConnectorHealthByProject(ctx, generated.ListConnectorHealthByProjectParams{
			ProjectID: pgtype.Text{String: project.ID, Valid: true}, OrgID: project.OrgID,
			SourceKind: tc.source, CompletedAt: since, Limit: 10,
		})
		if err != nil || len(rows) != 1 || rows[0].SourceKind != tc.source || rows[0].ConnectionID != tc.wantConnID {
			t.Fatalf("%s health rows = %#v (%v)", tc.source, rows, err)
		}
	}
}
