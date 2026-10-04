package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/config"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/hubspot"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/secretbox"
)

type hubspotOAuthRoundTrip func(*http.Request) (*http.Response, error)

func (f hubspotOAuthRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHubSpotOAuthConsentRefreshAndRevokeDB(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	other := seedProjectForIngest(t, pool)
	secondProject, err := generated.New(pool).CreateProject(ctx, generated.CreateProjectParams{
		ID: id.New(), OrgID: project.OrgID, Name: "second app", Slug: "second-" + id.New(),
		Platform: "other", PublicKey: id.New(), DsnID: id.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	userID := id.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,org_id,email,password_hash,role) VALUES($1,$2,$3,'test','admin')`, userID, project.OrgID, userID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	var exchanges, refreshes, revocations int
	badRefreshScope := false
	transport := &http.Client{Transport: hubspotOAuthRoundTrip(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		if r.URL.RawQuery != "" || form.Get("client_secret") != "test-client-secret" {
			t.Error("OAuth client secret not confined to form body")
		}
		body := ""
		status := http.StatusOK
		switch {
		case strings.HasSuffix(r.URL.Path, "/token") && form.Get("grant_type") == "authorization_code":
			exchanges++
			if form.Get("code") != "consent-code" {
				t.Error("unexpected authorization code")
			}
			body = `{"access_token":"access-1","refresh_token":"refresh-1","token_type":"bearer","hub_id":314159,"scopes":["oauth"],"expires_in":1800}`
		case strings.HasSuffix(r.URL.Path, "/token") && form.Get("grant_type") == "refresh_token":
			refreshes++
			if form.Get("refresh_token") != "refresh-move" && form.Get("refresh_token") != "refresh-2" {
				t.Error("stale or wrong refresh token")
			}
			if badRefreshScope {
				body = `{"access_token":"access-3","refresh_token":"refresh-3","token_type":"bearer","hub_id":314159,"scopes":["oauth","crm.objects.contacts.read"],"expires_in":1800}`
			} else {
				body = `{"access_token":"access-2","refresh_token":"refresh-2","token_type":"bearer","hub_id":314159,"scopes":["oauth"],"expires_in":1800}`
			}
		case strings.HasSuffix(r.URL.Path, "/token/revoke"):
			revocations++
			if form.Get("token") != "refresh-2" {
				t.Error("wrong token revoked")
			}
			status = http.StatusNoContent
		default:
			t.Errorf("unexpected provider request: %s", r.URL.Path)
			status = http.StatusNotFound
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	oauth := hubspot.OAuthClient{ClientID: "test-client-id", ClientSecret: "test-client-secret", RedirectURI: "https://flare.example/api/connectors/hubspot/callback", HTTPClient: transport}
	cipher := secretbox.New("oauth-db-test-key")
	s := serverForDB(pool)
	s.secrets = cipher
	s.cfg = config.Config{Environment: "production", BaseURL: "https://flare.example"}
	s.hubspotOAuth = oauth
	s.hubspotTokens = hubspot.TokenStore{Pool: pool, Cipher: cipher, Client: oauth}
	request := func(projectID string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/connectors/hubspot/start", strings.NewReader(`{"project_id":"`+projectID+`"}`))
		cx := context.WithValue(context.WithValue(context.WithValue(req.Context(), ctxOrgID, project.OrgID), ctxUserID, userID), ctxAuthSource, "session")
		return req.WithContext(cx)
	}
	bad := httptest.NewRecorder()
	s.handleHubSpotOAuthStart(bad, request(other.ID))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("cross-org start=%d", bad.Code)
	}
	var startAuditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE org_id=$1 AND action='connector.oauth_start'`, project.OrgID).Scan(&startAuditCount); err != nil || startAuditCount != 0 {
		t.Fatalf("rejected OAuth start wrote audit row: count=%d err=%v", startAuditCount, err)
	}
	started := httptest.NewRecorder()
	s.handleHubSpotOAuthStart(started, request(project.ID))
	if started.Code != http.StatusOK {
		t.Fatalf("start=%d %s", started.Code, started.Body.String())
	}
	var auditActor, auditTarget string
	if err := pool.QueryRow(ctx, `SELECT actor_user_id,target FROM audit_log WHERE org_id=$1 AND action='connector.oauth_start'`, project.OrgID).Scan(&auditActor, &auditTarget); err != nil || auditActor != userID || auditTarget != project.ID {
		t.Fatalf("OAuth start audit attribution: actor=%q target=%q err=%v", auditActor, auditTarget, err)
	}
	var startBody struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.Unmarshal(started.Body.Bytes(), &startBody); err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(startBody.AuthorizationURL)
	if err != nil || authURL.Host != "app.hubspot.com" || authURL.Query().Get("scope") != "oauth" {
		t.Fatalf("bad authorization URL: %s", startBody.AuthorizationURL)
	}
	state := authURL.Query().Get("state")
	cookie := started.Result().Cookies()[0]
	if cookie.SameSite != http.SameSiteLaxMode || !cookie.HttpOnly || !cookie.Secure || cookie.Path != hubspotCallbackPath {
		t.Fatal("callback cookie not cross-site-safe and scoped")
	}
	callbackURL := "/api/connectors/hubspot/callback?state=" + url.QueryEscape(state) + "&code=consent-code"
	wrong := httptest.NewRequest(http.MethodGet, callbackURL, nil)
	wrong.AddCookie(&http.Cookie{Name: hubspotTxCookie, Value: "wrong-cookie"})
	invalid := httptest.NewRecorder()
	s.handleHubSpotOAuthCallback(invalid, wrong)
	if !strings.Contains(invalid.Header().Get("Location"), "state_error") || exchanges != 0 {
		t.Fatal("wrong cookie reached provider")
	}
	call := httptest.NewRequest(http.MethodGet, callbackURL, nil)
	call.AddCookie(cookie)
	done := httptest.NewRecorder()
	s.handleHubSpotOAuthCallback(done, call)
	if done.Code != http.StatusSeeOther || done.Header().Get("Location") != "/settings?hubspot=connected" || exchanges != 1 {
		t.Fatalf("callback=%d %q exchanges=%d", done.Code, done.Header().Get("Location"), exchanges)
	}
	replay := httptest.NewRecorder()
	s.handleHubSpotOAuthCallback(replay, call)
	if !strings.Contains(replay.Header().Get("Location"), "state_error") || exchanges != 1 {
		t.Fatal("state replay reached provider")
	}
	// An explicit logout in another tab invalidates the short-lived pending
	// transaction in Postgres as well as clearing its Lax browser cookie.
	pending := httptest.NewRecorder()
	s.handleHubSpotOAuthStart(pending, request(project.ID))
	var pendingBody struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.Unmarshal(pending.Body.Bytes(), &pendingBody); err != nil {
		t.Fatal(err)
	}
	pendingURL, _ := url.Parse(pendingBody.AuthorizationURL)
	pendingCookie := pending.Result().Cookies()[0]
	s.sessions = auth.NewSessionManager(time.Hour, time.Hour, false)
	login := httptest.NewRecorder()
	s.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.sessions.Put(r.Context(), "user_id", userID)
		s.sessions.Put(r.Context(), "org_id", project.OrgID)
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/test-login", nil))
	sessionCookies := login.Result().Cookies()
	if len(sessionCookies) == 0 {
		t.Fatal("test session cookie not issued")
	}
	logoutRequest := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	logoutRequest.AddCookie(sessionCookies[0])
	logout := httptest.NewRecorder()
	s.sessions.LoadAndSave(http.HandlerFunc(s.handleLogout)).ServeHTTP(logout, logoutRequest)
	if logout.Code != http.StatusOK {
		t.Fatalf("logout = %d: %s", logout.Code, logout.Body.String())
	}
	cleared := false
	for _, c := range logout.Result().Cookies() {
		if c.Name == hubspotTxCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear HubSpot transaction cookie")
	}
	afterLogout := httptest.NewRequest(http.MethodGet, "/api/connectors/hubspot/callback?state="+url.QueryEscape(pendingURL.Query().Get("state"))+"&code=consent-code", nil)
	afterLogout.AddCookie(pendingCookie)
	denied := httptest.NewRecorder()
	s.handleHubSpotOAuthCallback(denied, afterLogout)
	if !strings.Contains(denied.Header().Get("Location"), "state_error") || exchanges != 1 {
		t.Fatal("callback completed after logout")
	}
	var connectionID, projectID, ciphertext string
	if err := pool.QueryRow(ctx, `SELECT id,project_id,secret_ciphertext FROM connector_connections WHERE org_id=$1 AND provider='hubspot' AND installation_id='314159'`, project.OrgID).Scan(&connectionID, &projectID, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if projectID != project.ID || !strings.HasPrefix(ciphertext, "enc:v1:") || strings.Contains(ciphertext, "access-1") || strings.Contains(ciphertext, "refresh-1") {
		t.Fatal("OAuth grant was not bound and encrypted")
	}
	// The generic admin PATCH surface must not transform the OAuth grant into
	// another provider or change its bound account/project/scopes/credential.
	for name, body := range map[string]string{
		"provider":      `{"provider":"github"}`,
		"installation":  `{"installation_id":"another-account"}`,
		"project":       `{"project_id":"` + secondProject.ID + `"}`,
		"credential":    `{"secret":"manual-token"}`,
		"scopes":        `{"scopes":["crm.objects.contacts.read"]}`,
		"revoke-status": `{"status":"revoked"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.handleUpdateConnectorConnection(rec, connectorProjectRequest(http.MethodPatch, "/api/connector-connections/"+connectionID, project.OrgID, body, map[string]string{"id": connectionID}))
			if rec.Code != http.StatusConflict {
				t.Fatalf("OAuth mutation = %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	var afterPatches string
	if err := pool.QueryRow(ctx, `SELECT secret_ciphertext FROM connector_connections WHERE id=$1`, connectionID).Scan(&afterPatches); err != nil || afterPatches != ciphertext {
		t.Fatal("denied metadata PATCH changed OAuth credential")
	}
	// Re-consenting the same account and project atomically replaces its grant
	// under the existing connection ID; it must not revoke the new grant after
	// a uniqueness conflict.
	reauthed, resultCode, err := s.saveHubSpotOAuth(ctx, hubspotOAuthTx{OrgID: project.OrgID, UserID: userID, ProjectID: project.ID, StartedAt: time.Now()}, hubspot.OAuthTokens{
		AccessToken: "access-reauth", RefreshToken: "refresh-reauth", TokenType: "bearer", HubID: 314159, Scopes: []string{"oauth"}, ExpiresIn: 1800,
	})
	if err != nil || resultCode != "connected" || reauthed != connectionID || revocations != 0 {
		t.Fatalf("same-account reauthorization = %q/%q/%v, revocations=%d", reauthed, resultCode, err, revocations)
	}
	if err := pool.QueryRow(ctx, `SELECT secret_ciphertext FROM connector_connections WHERE id=$1`, connectionID).Scan(&afterPatches); err != nil || afterPatches == ciphertext {
		t.Fatal("reauthorization did not rotate encrypted credential")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO connector_sync_states
		(id,org_id,connection_id,operation,state,last_success_at)
		VALUES ($1,$2,$3,'account_details','succeeded',now())`, id.New(), project.OrgID, connectionID); err != nil {
		t.Fatal(err)
	}
	oldReceiptID := id.New()
	if _, err := pool.Exec(ctx, `INSERT INTO connector_attempts
		(id,org_id,project_id,connection_id,source_kind,provider,operation,channel,outcome,status_class,elapsed_ms,attempted_at,completed_at)
		VALUES ($1,$2,$3,$4,'server_observed','hubspot','account_details','http','ok','2xx',1,now(),now())`,
		oldReceiptID, project.OrgID, project.ID, connectionID); err != nil {
		t.Fatal(err)
	}
	// A project chosen at a new consent is authoritative. The grant and
	// project move in one transaction under the existing connection ID.
	moved, moveCode, err := s.saveHubSpotOAuth(ctx, hubspotOAuthTx{OrgID: project.OrgID, UserID: userID, ProjectID: secondProject.ID, StartedAt: time.Now()}, hubspot.OAuthTokens{
		AccessToken: "access-move", RefreshToken: "refresh-move", TokenType: "bearer", HubID: 314159, Scopes: []string{"oauth"}, ExpiresIn: 1800,
	})
	if err != nil || moveCode != "moved" || moved != connectionID || revocations != 0 {
		t.Fatalf("cross-project reconsent = %q/%q/%v, revocations=%d", moved, moveCode, err, revocations)
	}
	var movedProject, movedState string
	var lastSuccess *time.Time
	if err := pool.QueryRow(ctx, `SELECT c.project_id,s.state,s.last_success_at FROM connector_connections c
		JOIN connector_sync_states s ON s.connection_id=c.id WHERE c.id=$1`, connectionID).
		Scan(&movedProject, &movedState, &lastSuccess); err != nil || movedProject != secondProject.ID || movedState != "idle" || lastSuccess != nil {
		t.Fatalf("move binding/state=%q/%q/%v err=%v", movedProject, movedState, lastSuccess, err)
	}
	var oldReceiptProject, racedReceiptProject string
	if err := pool.QueryRow(ctx, `SELECT project_id FROM connector_attempts WHERE id=$1`, oldReceiptID).Scan(&oldReceiptProject); err != nil || oldReceiptProject != project.ID {
		t.Fatalf("historical receipt moved=%q err=%v", oldReceiptProject, err)
	}
	// An in-flight probe may carry the pre-move project in memory. Postgres
	// derives the durable receipt project from the locked connection row.
	if err := pool.QueryRow(ctx, `INSERT INTO connector_attempts
		(id,org_id,project_id,connection_id,source_kind,provider,operation,channel,outcome,status_class,elapsed_ms,attempted_at,completed_at)
		VALUES ($1,$2,$3,$4,'server_observed','hubspot','account_details','http','ok','2xx',1,now(),now()) RETURNING project_id`,
		id.New(), project.OrgID, project.ID, connectionID).Scan(&racedReceiptProject); err != nil || racedReceiptProject != secondProject.ID {
		t.Fatalf("stale in-memory scope persisted=%q err=%v", racedReceiptProject, err)
	}
	now := time.Now().Add(29 * time.Minute)
	s.hubspotTokens.Now = func() time.Time { return now }
	token, err := s.hubspotTokens.Resolve(ctx, project.OrgID, connectionID)
	if err != nil || token != "access-2" || refreshes != 1 {
		t.Fatalf("refresh token=%q err=%v calls=%d", token, err, refreshes)
	}
	token, err = s.hubspotTokens.Resolve(ctx, project.OrgID, connectionID)
	if err != nil || token != "access-2" || refreshes != 1 {
		t.Fatal("fresh token refreshed again")
	}
	var afterGoodRefresh string
	if err := pool.QueryRow(ctx, `SELECT secret_ciphertext FROM connector_connections WHERE id=$1`, connectionID).Scan(&afterGoodRefresh); err != nil {
		t.Fatal(err)
	}
	// A refreshed grant that expands beyond account health is rejected and
	// does not overwrite the previously encrypted grant.
	badRefreshScope = true
	s.hubspotTokens.Now = func() time.Time { return now.Add(29 * time.Minute) }
	if _, err := s.hubspotTokens.Resolve(ctx, project.OrgID, connectionID); err == nil {
		t.Fatal("expanded refresh scope accepted")
	}
	var afterBadScope string
	if err := pool.QueryRow(ctx, `SELECT secret_ciphertext FROM connector_connections WHERE id=$1`, connectionID).Scan(&afterBadScope); err != nil || afterBadScope == "" {
		t.Fatal("encrypted grant unavailable after rejected refresh")
	}
	if afterBadScope != afterGoodRefresh {
		t.Fatal("expanded scope overwrote current refresh token")
	}
	// Revoked OAuth rows cannot be manually reactivated without new consent.
	if _, err := pool.Exec(ctx, `UPDATE connector_connections SET status='revoked' WHERE id=$1`, connectionID); err != nil {
		t.Fatal(err)
	}
	reactivate := httptest.NewRecorder()
	s.handleUpdateConnectorConnection(reactivate, connectorProjectRequest(http.MethodPatch, "/api/connector-connections/"+connectionID, project.OrgID, `{"status":"active"}`, map[string]string{"id": connectionID}))
	if reactivate.Code != http.StatusConflict {
		t.Fatalf("revoked OAuth reactivation = %d", reactivate.Code)
	}
	if _, err := pool.Exec(ctx, `UPDATE connector_connections SET status='active' WHERE id=$1`, connectionID); err != nil {
		t.Fatal(err)
	}
	del := connectorProjectRequest(http.MethodDelete, "/api/connector-connections/"+connectionID, project.OrgID, "", map[string]string{"id": connectionID})
	rec := httptest.NewRecorder()
	s.handleDeleteConnectorConnection(rec, del)
	if rec.Code != http.StatusNoContent || revocations != 1 {
		t.Fatalf("disconnect=%d revokes=%d", rec.Code, revocations)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM connector_connections WHERE id=$1`, connectionID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("connection remains=%d err=%v", remaining, err)
	}
}

func TestHubSpotOAuthStartRollsBackWhenAuditUnavailableDB(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	userID := id.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,org_id,email,password_hash,role) VALUES($1,$2,$3,'test','admin')`, userID, project.OrgID, userID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	s := serverForDB(pool)
	s.secrets = secretbox.New("oauth-audit-test-key")
	s.cfg = config.Config{Environment: "production", BaseURL: "https://flare.example"}
	s.hubspotOAuth = hubspot.OAuthClient{ClientID: "test-client-id", ClientSecret: "test-client-secret", RedirectURI: "https://flare.example/api/connectors/hubspot/callback"}
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/connectors/hubspot/start", strings.NewReader(`{"project_id":"`+project.ID+`"}`))
		cx := context.WithValue(context.WithValue(context.WithValue(req.Context(), ctxOrgID, project.OrgID), ctxUserID, userID), ctxAuthSource, "session")
		return req.WithContext(cx)
	}
	first := httptest.NewRecorder()
	s.handleHubSpotOAuthStart(first, request())
	if first.Code != http.StatusOK {
		t.Fatalf("baseline OAuth start = %d %s", first.Code, first.Body.String())
	}
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM hubspot_oauth_transactions`).Scan(&before); err != nil || before != 1 {
		t.Fatalf("baseline pending state: count=%d err=%v", before, err)
	}
	// A missing audit table simulates a failed durable audit write. A usable
	// consent URL/cookie must never escape while its state lacks attribution.
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_log RENAME TO audit_log_unavailable`); err != nil {
		t.Fatal(err)
	}
	failed := httptest.NewRecorder()
	s.handleHubSpotOAuthStart(failed, request())
	if failed.Code != http.StatusServiceUnavailable || len(failed.Result().Cookies()) != 0 || strings.Contains(failed.Body.String(), "authorization_url") {
		t.Fatalf("unattributed OAuth start escaped: status=%d cookies=%d body=%s", failed.Code, len(failed.Result().Cookies()), failed.Body.String())
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM hubspot_oauth_transactions`).Scan(&after); err != nil || after != before {
		t.Fatalf("failed audit retained pending state: before=%d after=%d err=%v", before, after, err)
	}
}
