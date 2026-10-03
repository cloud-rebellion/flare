package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/ratelimit"
	"github.com/bright-interaction/flare/internal/telemetry"
)

// authedReq builds a POST /api/mcp request whose context already carries the
// org + role the auth middleware would have set, so the handler can be tested
// in isolation.
func authedReq(t *testing.T, org, role, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/mcp", strings.NewReader(body))
	ctx := context.WithValue(r.Context(), ctxOrgID, org)
	ctx = context.WithValue(ctx, ctxRole, role)
	return r.WithContext(ctx)
}

func callMCP(t *testing.T, org, role, body string) mcpResponse {
	t.Helper()
	w := httptest.NewRecorder()
	(&Server{}).mcpHandler().ServeHTTP(w, authedReq(t, org, role, body))
	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
	}
	return resp
}

func TestMCPGetRejected(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxOrgID, "org1"))
	(&Server{}).mcpHandler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET should be 405, got %d", w.Code)
	}
}

func TestMCPUnauthenticated(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	(&Server{}).mcpHandler().ServeHTTP(w, r) // no org in context
	var resp mcpResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error == nil {
		t.Fatal("missing org should produce a JSON-RPC error")
	}
}

func TestMCPInitialize(t *testing.T) {
	resp := callMCP(t, "org1", "member", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if resp.Error != nil {
		t.Fatalf("initialize errored: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	var got struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	_ = json.Unmarshal(b, &got)
	if got.ProtocolVersion != mcpProtocol {
		t.Errorf("protocolVersion = %q, want %q", got.ProtocolVersion, mcpProtocol)
	}
	if got.ServerInfo.Name != "flare" {
		t.Errorf("serverInfo.name = %q, want flare", got.ServerInfo.Name)
	}
}

func TestMCPToolsList(t *testing.T) {
	resp := callMCP(t, "org1", "viewer", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("tools/list errored: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	var got struct {
		Tools []mcpTool `json:"tools"`
	}
	_ = json.Unmarshal(b, &got)
	names := map[string]bool{}
	for _, tl := range got.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"overview", "list_projects", "list_issues", "get_issue", "search_logs", "update_issue_status", "triage_issue"} {
		if !names[want] {
			t.Errorf("tools/list missing %q (got %v)", want, names)
		}
	}
}

// A viewer credential must be refused a write tool before dispatch, mirroring
// the REST member+ gate. This runs the gate without a DB because the denial
// short-circuits before the handler.
func TestMCPWriteGateDeniesViewer(t *testing.T) {
	resp := callMCP(t, "org1", "viewer", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"update_issue_status","arguments":{"issue_id":"x","status":"resolved"}}}`)
	if resp.Error != nil {
		t.Fatalf("tools/call transport errored: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	var got mcpToolResult
	_ = json.Unmarshal(b, &got)
	if !got.IsError {
		t.Fatal("viewer write should be an application error")
	}
	if len(got.Content) == 0 || !strings.Contains(got.Content[0].Text, "read-only") {
		t.Errorf("expected read-only denial, got %+v", got.Content)
	}
}

func TestMCPWriteGateMarksTenantKnownDenial(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxRole, "viewer")
	result, rpcErr := (&Server{}).mcpCall(ctx, "org1", json.RawMessage(`{"name":"update_issue_status","arguments":{}}`))
	if rpcErr != nil {
		t.Fatalf("role gate should return an application result: %+v", rpcErr)
	}
	got, ok := result.(mcpToolResult)
	if !ok || !got.Denied {
		t.Fatalf("role gate result did not carry the internal denial marker: %#v", result)
	}
}

func TestMCPUnknownTool(t *testing.T) {
	resp := callMCP(t, "org1", "member", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	b, _ := json.Marshal(resp.Result)
	var got mcpToolResult
	_ = json.Unmarshal(b, &got)
	if !got.IsError || len(got.Content) == 0 || !strings.Contains(got.Content[0].Text, "unknown tool") {
		t.Errorf("expected unknown-tool error, got %+v", got)
	}
}

func TestMCPNotificationNoContent(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).mcpHandler().ServeHTTP(w, authedReq(t, "org1", "member", `{"jsonrpc":"2.0","method":"initialized"}`))
	if w.Code != http.StatusNoContent {
		t.Fatalf("notification should be 204, got %d", w.Code)
	}
}

func TestMCPMethodNotFound(t *testing.T) {
	resp := callMCP(t, "org1", "member", `{"jsonrpc":"2.0","id":5,"method":"bogus/method"}`)
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("expected method-not-found (-32601), got %+v", resp.Error)
	}
}

// The dispatcher must not leak internal error detail to the client. Authored
// validation/not-found messages and the triage sentinels pass through; raw DB
// errors and wrapped provider bodies collapse to "internal error".
func TestMCPErrTextSanitizes(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"not-configured", errTriageNotConfigured, errTriageNotConfigured.Error()},
		{"endpoint-wraps-provider-body", fmt.Errorf("%w: 401 {\"error\":\"sk-leak\"}", errTriageEndpoint), errTriageEndpoint.Error()},
		{"user-error", userErr("project %q not found", "x"), `project "x" not found`},
		{"not-found", telemetry.ErrNotFound{What: "issue"}, "issue not found"},
		{"raw-db", errors.New(`ERROR: relation "issues" does not exist (SQLSTATE 42P01)`), "internal error"},
		{"raw-dial", errors.New("dial tcp 10.0.0.5:5432: connect: connection refused"), "internal error"},
	}
	for _, c := range cases {
		if got := s.mcpErrText("tool", c.err); got != c.want {
			t.Errorf("%s: mcpErrText = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRateLimitMCP(t *testing.T) {
	s := &Server{mcpLimiter: ratelimit.New(2, time.Minute)}
	h := s.rateLimitMCP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	hit := func() int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/mcp", nil)
		r = r.WithContext(context.WithValue(r.Context(), ctxOrgID, "org1"))
		h.ServeHTTP(w, r)
		return w.Code
	}
	if hit() != http.StatusOK || hit() != http.StatusOK {
		t.Fatal("first 2 requests within budget should pass")
	}
	if got := hit(); got != http.StatusTooManyRequests {
		t.Fatalf("3rd request should be 429, got %d", got)
	}
}
