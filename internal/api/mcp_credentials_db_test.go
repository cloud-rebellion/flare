package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/ratelimit"
)

func TestMCPProjectQueriesBoundOutputWithoutTruncatingTenantTotals(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	q := generated.New(pool)
	org, foreignOrg := id.New(), id.New()
	for _, tenant := range []string{org, foreignOrg} {
		if _, err := q.CreateOrg(ctx, generated.CreateOrgParams{ID: tenant, Name: "synthetic MCP bounds", Slug: tenant}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id,org_id,name,slug,public_key,dsn_id)
        SELECT 'mcp-project-'||lpad(i::text,4,'0'),$1,'project-'||i,'project-'||i,'synthetic-public-'||i,(800000000000+i)::text
        FROM generate_series(0,500) AS i`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id,org_id,name,slug,public_key,dsn_id)
        VALUES ('foreign-mcp-project',$1,'foreign-only','foreign-only','synthetic-foreign-public','900000000001')`, foreignOrg); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO issues (id,project_id,org_id,fingerprint,title,level,status)
        SELECT 'mcp-issue-'||i,'mcp-project-'||lpad(i::text,4,'0'),$1,'fingerprint-'||i,'synthetic issue','error','unresolved'
        FROM generate_series(0,500) AS i`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO issues (id,project_id,org_id,fingerprint,title,level,status)
        SELECT 'mcp-extra-'||i,'mcp-project-0500',$1,'extra-'||i,'synthetic extra','error','unresolved'
        FROM generate_series(1,2) AS i`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO issues (id,project_id,org_id,fingerprint,title,level,status)
        SELECT 'foreign-mcp-issue-'||i,'foreign-mcp-project',$1,'foreign-'||i,'foreign-only','error','unresolved'
        FROM generate_series(1,5) AS i`, foreignOrg); err != nil {
		t.Fatal(err)
	}
	s := serverForDB(pool)
	call := func(tenant, name string, target any) {
		t.Helper()
		cx := context.WithValue(ctx, ctxOrgID, tenant)
		cx = context.WithValue(cx, ctxRole, "viewer")
		params, _ := json.Marshal(map[string]any{"name": name, "arguments": map[string]any{}})
		result, rpcErr := s.mcpCall(cx, tenant, params)
		if rpcErr != nil {
			t.Fatal(rpcErr.Message)
		}
		r, ok := result.(mcpToolResult)
		if !ok || r.IsError || len(r.Content) != 1 {
			t.Fatal("MCP bound query failed")
		}
		if err := json.Unmarshal([]byte(r.Content[0].Text), target); err != nil {
			t.Fatal(err)
		}
		if tenant == org && (strings.Contains(r.Content[0].Text, "foreign-only") || strings.Contains(r.Content[0].Text, "synthetic-public-") || strings.Contains(r.Content[0].Text, "public_key") || strings.Contains(r.Content[0].Text, "dsn_id")) {
			t.Fatal("MCP query exposed foreign tenant data or ingest credentials")
		}
	}
	var listed []struct {
		ID string `json:"id"`
	}
	call(org, "list_projects", &listed)
	if len(listed) != maxMCPProjects {
		t.Fatalf("project response count=%d", len(listed))
	}
	for i, project := range listed {
		if project.ID != fmt.Sprintf("mcp-project-%04d", i) {
			t.Fatalf("project ordering unstable at%d: %s", i, project.ID)
		}
	}
	var overview struct {
		Unresolved   int64 `json:"unresolved"`
		NewToday     int64 `json:"new_today"`
		ProjectLimit int   `json:"project_limit"`
		Truncated    bool  `json:"projects_truncated"`
		Projects     []struct {
			ID    string `json:"project_id"`
			Name  string `json:"name"`
			Count int64  `json:"unresolved"`
		} `json:"projects"`
		TopIssues []json.RawMessage `json:"top_issues"`
		Trust     string            `json:"trust"`
	}
	call(org, "overview", &overview)
	if overview.Unresolved != 503 || overview.NewToday != 503 || overview.ProjectLimit != maxMCPProjects || !overview.Truncated || len(overview.Projects) != maxMCPProjects || len(overview.TopIssues) != 8 || overview.Trust == "" {
		t.Fatalf("overview lost total or cap: total=%d new=%d cap=%d truncated=%v rows=%d", overview.Unresolved, overview.NewToday, overview.ProjectLimit, overview.Truncated, len(overview.Projects))
	}
	if overview.Projects[0].ID != "mcp-project-0500" || overview.Projects[0].Count != 3 || overview.Projects[0].Name != "project-500" {
		t.Fatal("overview failed highest-count ordering or joined safe metadata")
	}
	for i, project := range overview.Projects[1:] {
		if project.ID != fmt.Sprintf("mcp-project-%04d", i) || project.Count != 1 {
			t.Fatalf("overview id tie order unstable at%d: %s", i, project.ID)
		}
	}
	// Existing dashboard/REST queries retain every tenant row and their shape.
	if rows, err := q.ListProjectsByOrg(ctx, org); err != nil || len(rows) != 501 {
		t.Fatalf("REST project query changed: %d %v", len(rows), err)
	}
	if rows, err := q.OverviewProjectUnresolved(ctx, org); err != nil || len(rows) != 501 {
		t.Fatalf("REST overview query changed: %d %v", len(rows), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE issues SET status='resolved' WHERE org_id=$1 AND project_id='mcp-project-0499'`, org); err != nil {
		t.Fatal(err)
	}
	call(org, "overview", &overview)
	if overview.Truncated || len(overview.Projects) != 500 || overview.Unresolved != 502 {
		t.Fatal("exact-cap control incorrectly reported truncation or clipped total")
	}
	call(foreignOrg, "overview", &overview)
	if overview.Truncated || overview.Unresolved != 5 || len(overview.Projects) != 1 || overview.Projects[0].ID != "foreign-mcp-project" {
		t.Fatal("small foreign tenant overview inherited another scope")
	}
}

func TestMCPCredentialLifecycleIsolationAndExplicitIdentity(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	q := generated.New(pool)
	org, _, projects := seedOrgProjectChannel(t, pool, 1)
	otherOrg, _, otherProjects := seedOrgProjectChannel(t, pool, 1)
	s := serverForDB(pool)
	s.cfg.BaseURL = "https://flare.example"
	s.cfg.Release = "v0.6.0+g" + strings.Repeat("a", 40)
	s.sessions = auth.NewSessionManager(time.Hour, time.Hour, false)
	s.mcpLimiter = ratelimit.New(mcpRatePerMin, time.Minute)
	session := func(org, role string) *http.Cookie {
		t.Helper()
		uid := id.New()
		if _, err := q.CreateUser(ctx, generated.CreateUserParams{ID: uid, OrgID: org, Email: uid + "@synthetic.invalid", PasswordHash: "unused-test-session", Role: role}); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		s.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.establishSession(r.Context(), uid, org)
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://flare.example/test-session", nil))
		for _, cookie := range w.Result().Cookies() {
			if cookie.Name == auth.SessionCookieName {
				return cookie
			}
		}
		t.Fatal("session cookie not created")
		return nil
	}
	admin, foreignAdmin, viewer := session(org, "admin"), session(otherOrg, "admin"), session(org, "viewer")
	// A strict test CSRF gate isolates its routing from token authentication;
	// explicit headers must never fall back to this browser session authority.
	csrfGate := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Header.Get("X-Test-CSRF") != "valid" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	handler := s.Routes(fstest.MapFS{"index.html": {Data: []byte("test")}}, csrfGate)
	call := func(method, path, body, authorization string, cookie *http.Cookie, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://flare.example"+path, strings.NewReader(body))
		if authorization != "" {
			r.Header.Set("Authorization", authorization)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if csrf {
			r.Header.Set("X-Test-CSRF", "valid")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	const capabilities = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_capabilities","arguments":{}}}`
	if w := call("POST", mcpEndpoint, capabilities, "", nil, false); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("MCP authentication discovery was masked: %d", w.Code)
	}
	for _, req := range []string{`{"purpose":"mcp","expires_in_days":-1}`, `{"purpose":"mcp","expires_in_days":91}`, `{"purpose":"mcp","role":"owner"}`, `{"purpose":"global"}`} {
		if w := call("POST", "/api/keys", req, "", admin, true); w.Code != 400 {
			t.Fatalf("invalid MCP credential request accepted: %d", w.Code)
		}
	}
	if w := call("POST", "/api/keys", `{"purpose":"mcp"}`, "", viewer, true); w.Code != 403 {
		t.Fatalf("viewer issued a credential: %d", w.Code)
	}
	created := call("POST", "/api/keys", `{"purpose":"mcp","name":"dedicated synthetic MCP"}`, "", admin, true)
	var minted struct {
		ID, Key, Role, Purpose string
		ExpiresAt              time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &minted); err != nil || created.Code != 201 || minted.Purpose != "mcp" || minted.Role != "viewer" || minted.Key == "" || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("dedicated issuance contract failed: status=%d err=%v", created.Code, err)
	}
	var storedHash, purpose string
	var createdAt, expiresAt time.Time
	if err := pool.QueryRow(ctx, `SELECT key_hash,purpose,created_at,expires_at FROM api_keys WHERE id=$1 AND org_id=$2`, minted.ID, org).Scan(&storedHash, &purpose, &createdAt, &expiresAt); err != nil || storedHash != auth.HashAPIKey(minted.Key) || storedHash == minted.Key || purpose != "mcp" || expiresAt.Sub(createdAt) != 30*24*time.Hour || !expiresAt.Equal(minted.ExpiresAt) {
		t.Fatalf("hashed bounded issuance failed: %v", err)
	}
	listed := call("GET", "/api/keys", "", "", admin, false)
	if listed.Code != 200 || strings.Contains(listed.Body.String(), minted.Key) || strings.Contains(listed.Body.String(), storedHash) || !strings.Contains(listed.Body.String(), `"purpose":"mcp"`) || !strings.Contains(listed.Body.String(), `"expires_at":`) {
		t.Fatal("key list lost expiry/purpose or exposed a secret")
	}
	for _, origins := range [][]string{{"https://foreign.invalid"}, {"null"}, {"https://flare.example/"}, {"https://flare.example", "https://flare.example"}} {
		req := httptest.NewRequest("POST", "https://flare.example"+mcpEndpoint, strings.NewReader(capabilities))
		req.Header.Set("Authorization", "Bearer "+minted.Key)
		for _, origin := range origins {
			req.Header.Add("Origin", origin)
		}
		req.Host = "foreign.invalid"
		req.Header.Set("X-Forwarded-Host", "foreign.invalid")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("foreign/malformed/duplicate Origin reached authenticated MCP: %d", w.Code)
		}
	}
	var untouched bool
	if err := pool.QueryRow(ctx, `SELECT last_used_at IS NULL FROM api_keys WHERE id=$1`, minted.ID).Scan(&untouched); err != nil || !untouched {
		t.Fatal("Origin refusal reached bearer authentication")
	}
	s.cfg.MCPAllowedOrigins = []string{"https://alternate.example"}
	for _, origin := range []string{"https://flare.example", "https://alternate.example"} {
		req := httptest.NewRequest("POST", "https://flare.example"+mcpEndpoint, strings.NewReader(capabilities))
		req.Header.Set("Authorization", "Bearer "+minted.Key)
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("configured Origin refused: %d", w.Code)
		}
	}
	accepted := call("POST", mcpEndpoint, capabilities, "Bearer "+minted.Key, foreignAdmin, false)
	var rpc struct {
		Result mcpToolResult `json:"result"`
	}
	if err := json.Unmarshal(accepted.Body.Bytes(), &rpc); err != nil || accepted.Code != 200 || rpc.Result.IsError || len(rpc.Result.Content) != 1 {
		t.Fatalf("MCP token rejected: %d %v", accepted.Code, err)
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &info); err != nil {
		t.Fatal(err)
	}
	var identity map[string]json.RawMessage
	if err := json.Unmarshal(info["identity"], &identity); err != nil {
		t.Fatal(err)
	}
	var gotOrg, gotRole, gotPurpose, gotSource, gotServer, gotRelease, gotScope string
	var gotExpiry time.Time
	_ = json.Unmarshal(identity["org_id"], &gotOrg)
	_ = json.Unmarshal(identity["role"], &gotRole)
	_ = json.Unmarshal(identity["credential_purpose"], &gotPurpose)
	_ = json.Unmarshal(identity["auth_source"], &gotSource)
	_ = json.Unmarshal(identity["expires_at"], &gotExpiry)
	_ = json.Unmarshal(info["server_name"], &gotServer)
	_ = json.Unmarshal(info["release"], &gotRelease)
	_ = json.Unmarshal(info["scope"], &gotScope)
	if gotOrg != org || gotRole != "viewer" || gotPurpose != "mcp" || gotSource != "api_key" || !gotExpiry.Equal(minted.ExpiresAt) || gotServer != "flare" || gotRelease != s.cfg.Release || gotScope != "all_projects_in_workspace" || strings.Contains(accepted.Body.String(), minted.Key) || strings.Contains(accepted.Body.String(), storedHash) || strings.Contains(accepted.Body.String(), "update_issue_status") {
		t.Fatal("ambient foreign/admin cookie replaced the explicit MCP identity or metadata leaked secrets")
	}
	for _, authorization := range []string{"Bearer invalid", "Basic invalid", "Bearer "} {
		if w := call("POST", mcpEndpoint, capabilities, authorization, admin, false); w.Code != 401 {
			t.Fatalf("invalid explicit auth fell back to session: %d", w.Code)
		}
	}
	duplicate := httptest.NewRequest("POST", "https://flare.example"+mcpEndpoint, strings.NewReader(capabilities))
	duplicate.Header.Add("Authorization", "Bearer "+minted.Key)
	duplicate.Header.Add("Authorization", "Bearer invalid")
	duplicate.AddCookie(admin)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, duplicate)
	if w.Code != 401 {
		t.Fatalf("ambiguous duplicate Authorization inherited a session: %d", w.Code)
	}
	if w := call("POST", mcpEndpoint, capabilities, "", admin, false); w.Code != 403 {
		t.Fatalf("cookie-only MCP bypassed CSRF: %d", w.Code)
	}
	if w := call("POST", mcpEndpoint, capabilities, "", admin, true); w.Code != 200 {
		t.Fatalf("protected browser MCP refused: %d", w.Code)
	}
	if w := call("GET", "/api/mcp/status", "", "", viewer, false); w.Code != 200 || strings.Contains(w.Body.String(), "update_issue_status") || !strings.Contains(w.Body.String(), `"oauth_supported":false`) {
		t.Fatal("viewer setup status lost its authority or advertised OAuth")
	}
	for _, path := range []string{"/api/projects", "/api/projects/" + projects[0], "/api/mcp/status", "/api/keys"} {
		if w := call("GET", path, "", "Bearer "+minted.Key, admin, false); w.Code != 403 {
			t.Fatalf("MCP token used on REST %s: %d", path, w.Code)
		}
	}
	write := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"update_issue_status","arguments":{"issue_id":"missing","status":"resolved"}}}`
	if w := call("POST", mcpEndpoint, write, "Bearer "+minted.Key, admin, false); w.Code != 200 || !strings.Contains(w.Body.String(), "read-only") {
		t.Fatal("viewer token inherited admin write authority")
	}
	foreignProject := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_issues","arguments":{"project":"` + otherProjects[0] + `"}}}`
	if w := call("POST", mcpEndpoint, foreignProject, "Bearer "+minted.Key, nil, false); w.Code != 200 || !strings.Contains(w.Body.String(), "not found") {
		t.Fatal("MCP project query bypassed organization scope")
	}
	if w := call("DELETE", "/api/keys/"+minted.ID, "", "", foreignAdmin, true); w.Code != 404 {
		t.Fatalf("foreign tenant revoked token: %d", w.Code)
	}
	if w := call("DELETE", "/api/keys/"+minted.ID, "", "", admin, true); w.Code != 204 {
		t.Fatalf("token revoke refused: %d", w.Code)
	}
	if w := call("POST", mcpEndpoint, capabilities, "Bearer "+minted.Key, admin, false); w.Code != 401 {
		t.Fatalf("revoked token fell back to admin cookie: %d", w.Code)
	}
	// Existing CreateAPIKey fixtures deliberately omit purpose and retain their
	// default API authority for REST and MCP. No fixture-wide empty-string edit.
	legacy := "synthetic-api-" + id.New()
	if _, err := q.CreateAPIKey(ctx, generated.CreateAPIKeyParams{ID: id.New(), OrgID: org, Name: "legacy", KeyHash: auth.HashAPIKey(legacy), KeyPrefix: "synthetic", Role: "viewer"}); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/api/projects", "", "Bearer "+legacy, nil, false); w.Code != 200 {
		t.Fatalf("legacy REST compatibility: %d", w.Code)
	}
	if w := call("POST", mcpEndpoint, capabilities, "Bearer "+legacy, nil, false); w.Code != 200 {
		t.Fatalf("legacy MCP compatibility: %d", w.Code)
	}
	second := call("POST", "/api/keys", `{"purpose":"mcp","expires_in_days":90,"role":"member"}`, "", admin, true)
	var expiring struct{ ID, Key string }
	if json.Unmarshal(second.Body.Bytes(), &expiring) != nil || second.Code != 201 {
		t.Fatal("maximum MCP expiry refused")
	}
	if _, err := pool.Exec(ctx, `UPDATE api_keys SET created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE id=$1`, expiring.ID); err != nil {
		t.Fatal(err)
	}
	if w := call("POST", mcpEndpoint, capabilities, "Bearer "+expiring.Key, admin, false); w.Code != 401 {
		t.Fatalf("expired token inherited session: %d", w.Code)
	}
	if _, err := pool.Exec(ctx, `UPDATE api_keys SET expires_at=NULL WHERE id=$1`, expiring.ID); err == nil {
		t.Fatal("database allowed permanent MCP credential")
	}
	if _, err := pool.Exec(ctx, `UPDATE api_keys SET expires_at=created_at+interval '91 days' WHERE id=$1`, expiring.ID); err == nil {
		t.Fatal("database allowed oversized MCP lifetime")
	}
}
