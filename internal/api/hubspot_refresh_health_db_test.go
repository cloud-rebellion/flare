package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/connectorcheck"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/hubspot"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/secretbox"
	"github.com/jackc/pgx/v5/pgtype"
)

type uncalledAccountProbe struct{ calls int }

func (p *uncalledAccountProbe) Check(context.Context, string) (connectorcheck.Result, error) {
	p.calls++
	return connectorcheck.Result{}, errors.New("account probe must not run after a rejected refresh")
}

// A rejected refresh grant occurs before the account probe. The check must
// still leave one tenant-bound server receipt usable by an auth-expiry rule.
func TestHubSpotRefreshFailureProducesScopedCheckReceiptAndAlertDB(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	foreignOrg := seedProjectForIngest(t, pool)
	foreignProject, err := generated.New(pool).CreateProject(ctx, generated.CreateProjectParams{
		ID: id.New(), OrgID: project.OrgID, Name: "other", Slug: "other-" + id.New(),
		Platform: "other", PublicKey: id.New(), DsnID: id.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	connectionID := id.New()
	cipher := secretbox.New("refresh-health-test-key")
	stored, err := hubspot.NewOAuthCredential(project.OrgID, project.ID, connectionID, hubspot.OAuthTokens{
		AccessToken: "expired-access", RefreshToken: "rejected-refresh", HubID: 314159, ExpiresIn: 1800,
	}, time.Now().UTC().Add(-time.Hour)).Encode(cipher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generated.New(pool).CreateConnectorConnection(ctx, generated.CreateConnectorConnectionParams{
		ID: connectionID, OrgID: project.OrgID, ProjectID: pgtype.Text{String: project.ID, Valid: true},
		Provider: "hubspot", InstallationID: "314159", Status: "active",
		Scopes: json.RawMessage(`["oauth"]`), SecretCiphertext: stored,
	}); err != nil {
		t.Fatal(err)
	}
	refreshes := 0
	providerClient := &http.Client{Transport: hubspotOAuthRoundTrip(func(r *http.Request) (*http.Response, error) {
		refreshes++
		if r.URL.Path != "/oauth/2026-03/token" || r.URL.RawQuery != "" {
			t.Fatalf("unexpected refresh endpoint: %s", r.URL.String())
		}
		return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant","error_description":"provider-private-detail"}`))}, nil
	})}
	oauth := hubspot.OAuthClient{ClientID: "client", ClientSecret: "secret", RedirectURI: "https://flare.example/api/connectors/hubspot/callback", HTTPClient: providerClient}
	s := serverForDB(pool)
	s.secrets = cipher
	s.hubspotTokens = hubspot.TokenStore{Pool: pool, Cipher: cipher, Client: oauth}
	probe := &uncalledAccountProbe{}
	s.hubspotProbe = probe
	integration.SetRecorderWithError(s.recordConnectorAttempt)
	t.Cleanup(func() { integration.SetRecorder(nil) })
	check := func(orgID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleCheckConnectorConnection(rec, connectorProjectRequest(http.MethodPost, "/api/connector-connections/"+connectionID+"/check", orgID, "", map[string]string{"id": connectionID}))
		return rec
	}
	first := check(project.OrgID)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"error_code":"auth_expired"`) ||
		!strings.Contains(first.Body.String(), `"status_class":"4xx"`) || !strings.Contains(first.Body.String(), `"receipt_status":"accepted"`) ||
		strings.Contains(first.Body.String(), "provider-private-detail") || probe.calls != 0 || refreshes != 1 {
		t.Fatalf("refresh check=%d %s, probe calls=%d refreshes=%d", first.Code, first.Body.String(), probe.calls, refreshes)
	}
	var receiptOrg, receiptProject, receiptConnection, source, operation, channel, outcome, statusClass, errorCode string
	if err := pool.QueryRow(ctx, `SELECT org_id, COALESCE(project_id,''), COALESCE(connection_id,''), source_kind,
		operation, channel, outcome, status_class, error_code FROM connector_attempts WHERE connection_id=$1`, connectionID).
		Scan(&receiptOrg, &receiptProject, &receiptConnection, &source, &operation, &channel, &outcome, &statusClass, &errorCode); err != nil {
		t.Fatal(err)
	}
	if receiptOrg != project.OrgID || receiptProject != project.ID || receiptConnection != connectionID || source != "server_observed" ||
		operation != "account_details" || channel != "http" || outcome != "error" || statusClass != "4xx" || errorCode != "auth_expired" {
		t.Fatalf("refresh receipt scope/outcome = %q/%q/%q/%q/%q/%q/%q/%q/%q", receiptOrg, receiptProject, receiptConnection, source, operation, channel, outcome, statusClass, errorCode)
	}
	for _, scopedProject := range []string{foreignProject.ID, foreignOrg.ID} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM connector_attempts WHERE connection_id=$1 AND project_id=$2`, connectionID, scopedProject).Scan(&count); err != nil || count != 0 {
			t.Fatalf("foreign project %s receipt count=%d err=%v", scopedProject, count, err)
		}
	}
	alertBody := `{"name":"HubSpot grant expired","type":"connector_error_rate","connector_provider":"hubspot","connector_operation":"account_details","connector_channel":"http","connector_error_code":"auth_expired","connector_connection_id":"` + connectionID + `","connector_source_kind":"server_observed","comparison":"gte","threshold_value":1,"window_minutes":5}`
	created := httptest.NewRecorder()
	s.handleCreateAlertRule(created, connectorProjectRequest(http.MethodPost, "/api/projects/"+project.ID+"/alert-rules", project.OrgID, alertBody, map[string]string{"id": project.ID}))
	if created.Code != http.StatusCreated {
		t.Fatalf("create auth-expiry rule=%d %s", created.Code, created.Body.String())
	}
	rules, err := s.q.ListWatchdogRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fired := false
	for _, rule := range rules {
		if rule.ConnectorConnectionID == connectionID && rule.ConnectorErrorCode == "auth_expired" {
			fired = s.watchdogReason(ctx, rule, time.Now().UTC()) != ""
		}
	}
	if !fired {
		t.Fatal("auth-expiry rule did not evaluate the refresh receipt")
	}
	if foreign := check(foreignOrg.OrgID); foreign.Code != http.StatusNotFound || refreshes != 1 {
		t.Fatalf("foreign organization check=%d refreshes=%d", foreign.Code, refreshes)
	}
	// Provider failure and receipt durability remain separate. A failed write
	// must be visible without repeating the account probe or claiming success.
	integration.SetRecorderWithError(func(context.Context, integration.Attempt) error { return errors.New("receipt store unavailable") })
	second := check(project.OrgID)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"error_code":"auth_expired"`) ||
		!strings.Contains(second.Body.String(), `"receipt_status":"unavailable"`) || probe.calls != 0 || refreshes != 2 {
		t.Fatalf("unavailable receipt check=%d %s, probe calls=%d refreshes=%d", second.Code, second.Body.String(), probe.calls, refreshes)
	}
	// An infrastructure failure in the token store is not a provider outcome.
	s.hubspotTokens.Cipher = nil
	infra := check(project.OrgID)
	if infra.Code != http.StatusServiceUnavailable || probe.calls != 0 || refreshes != 2 {
		t.Fatalf("token-store infrastructure check=%d %s, probe calls=%d refreshes=%d", infra.Code, infra.Body.String(), probe.calls, refreshes)
	}
}
