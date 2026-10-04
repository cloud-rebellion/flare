package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/connectorcheck"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/secretbox"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type projectBoundProbe struct {
	server *Server
	scope  integration.Scope
}

func (p *projectBoundProbe) Check(ctx context.Context, _ string) (connectorcheck.Result, error) {
	p.scope = integration.ScopeFrom(ctx)
	now := time.Now().UTC()
	err := p.server.recordConnectorAttempt(ctx, integration.Attempt{
		Scope: p.scope, Provider: "hubspot", Operation: "account_details", Channel: "http",
		Outcome: "error", StatusClass: "4xx", ErrorCode: "auth_expired",
		AttemptedAt: now.Add(-10 * time.Millisecond), CompletedAt: now,
	})
	return connectorcheck.Result{
		Outcome: "error", StatusClass: "4xx", ErrorCode: "auth_expired",
		ReceiptStatus: integration.ReceiptStatus(err),
	}, nil
}

func connectorProjectRequest(method, path, org, body string, params map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxOrgID, org)
	route := chi.NewRouteContext()
	for key, value := range params {
		route.URLParams.Add(key, value)
	}
	return req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, route))
}

// The real migrated database, admin handlers, recorder, project health read,
// sync projection and alert creation must agree on one tenant-owned binding.
func TestHubSpotProjectBindingProducesProjectHealthAndAlerts(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	projectA := seedProjectForIngest(t, pool)
	projectOtherOrg := seedProjectForIngest(t, pool)
	q := generated.New(pool)
	projectB, err := q.CreateProject(ctx, generated.CreateProjectParams{
		ID: id.New(), OrgID: projectA.OrgID, Name: "other-app", Slug: "other-app-" + id.New(),
		Platform: "other", PublicKey: id.New(), DsnID: id.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := serverForDB(pool)
	s.secrets = secretbox.New("project-binding-test-key")
	create := func(projectID string) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(map[string]any{
			"provider": "hubspot", "installation_id": "hubspot-prod", "status": "active",
			"secret": "private-token-test", "project_id": projectID,
		})
		rec := httptest.NewRecorder()
		s.handleCreateConnectorConnection(rec, connectorProjectRequest(http.MethodPost, "/api/connector-connections", projectA.OrgID, string(payload), nil))
		return rec
	}
	if rec := create(""); rec.Code != http.StatusBadRequest {
		t.Fatalf("unbound active HubSpot = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := create(projectOtherOrg.ID); rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-org project binding = %d: %s", rec.Code, rec.Body.String())
	}
	rec := create(projectA.ID)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create bound HubSpot = %d: %s", rec.Code, rec.Body.String())
	}
	var connection connectorConnectionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &connection); err != nil {
		t.Fatal(err)
	}
	if connection.ProjectID != projectA.ID || !connection.CredentialConfigured || strings.Contains(rec.Body.String(), "private-token-test") {
		t.Fatalf("bound connection response = %s", rec.Body.String())
	}
	probe := &projectBoundProbe{server: s}
	s.hubspotProbe = probe
	check := httptest.NewRecorder()
	s.handleCheckConnectorConnection(check, connectorProjectRequest(http.MethodPost, "/api/connector-connections/"+connection.ID+"/check", projectA.OrgID, "", map[string]string{"id": connection.ID}))
	if check.Code != http.StatusOK || !strings.Contains(check.Body.String(), `"receipt_status":"accepted"`) {
		t.Fatalf("check = %d: %s", check.Code, check.Body.String())
	}
	if probe.scope.OrgID != projectA.OrgID || probe.scope.ProjectID != projectA.ID || probe.scope.ConnectionID != connection.ID {
		t.Fatalf("provider scope = %#v", probe.scope)
	}
	readHealth := func(projectID, orgID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleListProjectConnectorHealth(rec, connectorProjectRequest(http.MethodGet, "/api/projects/"+projectID+"/connector-health", orgID, "", map[string]string{"id": projectID}))
		return rec
	}
	if got := readHealth(projectA.ID, projectA.OrgID); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), connection.ID) || !strings.Contains(got.Body.String(), `"source_kind":"server_observed"`) {
		t.Fatalf("project A connector health = %d: %s", got.Code, got.Body.String())
	}
	if got := readHealth(projectB.ID, projectA.OrgID); got.Code != http.StatusOK || strings.TrimSpace(got.Body.String()) != "[]" {
		t.Fatalf("project B inherited connector health = %d: %s", got.Code, got.Body.String())
	}
	if got := readHealth(projectOtherOrg.ID, projectA.OrgID); got.Code != http.StatusNotFound {
		t.Fatalf("cross-org connector health = %d: %s", got.Code, got.Body.String())
	}

	// Even if a future in-process adapter supplies the wrong project, the DB
	// derives attribution from the connection row before accepting the receipt.
	now := time.Now().UTC()
	if err := s.recordConnectorAttempt(ctx, integration.Attempt{
		Scope:    integration.Scope{OrgID: projectA.OrgID, ProjectID: projectB.ID, ConnectionID: connection.ID},
		Provider: "hubspot", Operation: "account_details", Channel: "worker", Outcome: "ok", StatusClass: "2xx",
		AttemptedAt: now, CompletedAt: now,
	}); err != nil {
		t.Fatalf("authoritative connector attribution: %v", err)
	}
	var boundCount, misboundCount int
	if err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE project_id=$1), count(*) FILTER (WHERE project_id=$2)
		FROM connector_attempts WHERE org_id=$3 AND connection_id=$4`, projectA.ID, projectB.ID, projectA.OrgID, connection.ID).Scan(&boundCount, &misboundCount); err != nil {
		t.Fatal(err)
	}
	if boundCount != 2 || misboundCount != 0 {
		t.Fatalf("registered receipts attributed to A/B = %d/%d", boundCount, misboundCount)
	}
	if err := s.recordConnectorAttempt(ctx, integration.Attempt{
		Scope:    integration.Scope{OrgID: projectA.OrgID, ConnectionID: connection.ID},
		Provider: "github", Operation: "user", Channel: "http", Outcome: "ok", StatusClass: "2xx",
		AttemptedAt: now, CompletedAt: now,
	}); err == nil {
		t.Fatal("registered connection accepted a different provider")
	}

	_, err = q.EnsureConnectorSyncState(ctx, generated.EnsureConnectorSyncStateParams{
		ID: id.New(), OrgID: projectA.OrgID, ConnectionID: connection.ID, Operation: "account_details",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := q.TryClaimConnectorSync(ctx, generated.TryClaimConnectorSyncParams{
		OrgID: projectA.OrgID, ConnectionID: connection.ID, Operation: "account_details",
		LeaseOwner: "test-worker", LeaseExpiresAt: pgtype.Timestamptz{Time: now.Add(time.Minute), Valid: true},
	})
	if err != nil || lease.State != "running" {
		t.Fatalf("sync lease = %#v/%v", lease, err)
	}
	_, err = q.CompleteConnectorSync(ctx, generated.CompleteConnectorSyncParams{
		OrgID: projectA.OrgID, ConnectionID: connection.ID, Operation: "account_details",
		LeaseOwner: "test-worker", Outcome: "error", LastErrorCode: "auth_expired",
		NextRunAt: pgtype.Timestamptz{Time: now.Add(time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	readSync := func(projectID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleListProjectConnectorSyncHealth(rec, connectorProjectRequest(http.MethodGet, "/api/projects/"+projectID+"/connector-sync-health", projectA.OrgID, "", map[string]string{"id": projectID}))
		return rec
	}
	if got := readSync(projectA.ID); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), connection.ID) {
		t.Fatalf("project A sync health = %d: %s", got.Code, got.Body.String())
	}
	if got := readSync(projectB.ID); got.Code != http.StatusOK || strings.TrimSpace(got.Body.String()) != "[]" {
		t.Fatalf("project B inherited sync state = %d: %s", got.Code, got.Body.String())
	}
	for _, tc := range []struct {
		projectID  string
		wantFailed int64
	}{{projectA.ID, 1}, {projectB.ID, 0}} {
		stats, err := q.GetConnectorSyncHealthStats(ctx, generated.GetConnectorSyncHealthStatsParams{
			OrgID: projectA.OrgID, ProjectID: tc.projectID, ConnectionID: connection.ID,
			Provider: "hubspot", Operation: "account_details",
			Cutoff: pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
		})
		if err != nil || stats.FailedCount != tc.wantFailed {
			t.Fatalf("project %s failed sync stats = %#v/%v", tc.projectID, stats, err)
		}
	}
	alertBody := `{"name":"HubSpot account check failed","type":"connector_sync_health","connector_provider":"hubspot","connector_operation":"account_details","connector_connection_id":"` + connection.ID + `","connector_sync_mode":"failed","threshold":1,"window_minutes":5}`
	for _, tc := range []struct {
		projectID  string
		wantStatus int
	}{{projectB.ID, http.StatusBadRequest}, {projectA.ID, http.StatusCreated}, {projectOtherOrg.ID, http.StatusNotFound}} {
		rec := httptest.NewRecorder()
		s.handleCreateAlertRule(rec, connectorProjectRequest(http.MethodPost, "/api/projects/"+tc.projectID+"/alert-rules", projectA.OrgID, alertBody, map[string]string{"id": tc.projectID}))
		if rec.Code != tc.wantStatus {
			t.Fatalf("project %s alert = %d, want %d: %s", tc.projectID, rec.Code, tc.wantStatus, rec.Body.String())
		}
	}
	// Simulate a pre-migration, incorrectly routed rule. It remains stored, but
	// the watchdog evaluates only the rule whose project owns the connection.
	if _, err := pool.Exec(ctx, `INSERT INTO alert_rules
		(id, project_id, org_id, name, type, threshold, window_minutes,
		 connector_provider, connector_operation, connector_connection_id, connector_sync_mode)
		VALUES ($1,$2,$3,'historical misroute','connector_sync_health',1,5,'hubspot','account_details',$4,'failed')`,
		id.New(), projectB.ID, projectA.OrgID, connection.ID); err != nil {
		t.Fatal(err)
	}
	rules, err := q.ListWatchdogRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ownerFires, misroutedFires int
	for _, rule := range rules {
		if rule.Type != "connector_sync_health" || s.watchdogReason(ctx, rule, time.Now().UTC()) == "" {
			continue
		}
		if rule.ProjectID == projectA.ID {
			ownerFires++
		}
		if rule.ProjectID == projectB.ID {
			misroutedFires++
		}
	}
	if ownerFires != 1 || misroutedFires != 0 {
		t.Fatalf("watchdog project routing owner/misroute = %d/%d", ownerFires, misroutedFires)
	}
}

// Existing org-level rows keep their credential and history. An explicit
// tenant-checked PATCH assigns future server receipts to one project.
func TestHistoricalConnectorBindingPreservesSecretAndAttempts(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	projectA := seedProjectForIngest(t, pool)
	otherOrgProject := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	q := s.q
	s.secrets = secretbox.New("historical-binding-test-key")
	connection, err := q.CreateConnectorConnection(ctx, generated.CreateConnectorConnectionParams{
		ID: id.New(), OrgID: projectA.OrgID, Provider: "hubspot", InstallationID: "legacy-install",
		Status: "active", Scopes: json.RawMessage(`[]`), SecretCiphertext: s.secrets.Encrypt("keep-this-token"),
	})
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	if err := s.recordConnectorAttempt(ctx, integration.Attempt{
		Scope:    integration.Scope{OrgID: projectA.OrgID, ConnectionID: connection.ID},
		Provider: "hubspot", Operation: "account_details", Channel: "http", Outcome: "ok", StatusClass: "2xx",
		AttemptedAt: before, CompletedAt: before,
	}); err != nil {
		t.Fatal(err)
	}
	patch := func(projectID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleUpdateConnectorConnection(rec, connectorProjectRequest(http.MethodPatch, "/api/connector-connections/"+connection.ID, projectA.OrgID, `{"project_id":"`+projectID+`"}`, map[string]string{"id": connection.ID}))
		return rec
	}
	if rec := patch(otherOrgProject.ID); rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-org patch = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := patch(projectA.ID); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), projectA.ID) {
		t.Fatalf("bind historical connection = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := patch(otherOrgProject.ID); rec.Code != http.StatusConflict {
		t.Fatalf("rebind historical connection = %d: %s", rec.Code, rec.Body.String())
	}
	secret, err := q.GetConnectorConnectionSecret(ctx, generated.GetConnectorConnectionSecretParams{ID: connection.ID, OrgID: projectA.OrgID})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.secrets.Decrypt(secret)
	if err != nil || plain != "keep-this-token" {
		t.Fatalf("credential changed during binding: %v", err)
	}
	var oldProject pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT project_id FROM connector_attempts WHERE connection_id=$1 ORDER BY completed_at LIMIT 1`, connection.ID).Scan(&oldProject); err != nil || oldProject.Valid {
		t.Fatalf("historical project attribution changed: %#v/%v", oldProject, err)
	}
	if err := s.recordConnectorAttempt(ctx, integration.Attempt{
		Scope:    integration.Scope{OrgID: projectA.OrgID, ConnectionID: connection.ID},
		Provider: "hubspot", Operation: "account_details", Channel: "http", Outcome: "ok", StatusClass: "2xx",
		AttemptedAt: before.Add(time.Second), CompletedAt: before.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	var newProject pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT project_id FROM connector_attempts WHERE connection_id=$1 ORDER BY completed_at DESC LIMIT 1`, connection.ID).Scan(&newProject); err != nil || !newProject.Valid || newProject.String != projectA.ID {
		t.Fatalf("new project attribution = %#v/%v", newProject, err)
	}
}
