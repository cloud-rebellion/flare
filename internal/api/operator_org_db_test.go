package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/retention"
)

// The operator workspace (the one allowed to change instance-wide retention)
// used to be "the oldest org". Its owner deleting it promoted the next-oldest
// tenant to operator and cascaded away the retention audit trail (audit P1-1).
func TestOperatorWorkspaceCannotBeDeletedWhileTenantsExist(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	s := serverForDB(pool)
	s.cfg.BaseURL = "https://flare.example"
	s.cfg.RetentionDays = 30
	s.cfg.ColdRetentionMode = retention.ColdForever
	s.sessions = auth.NewSessionManager(time.Hour, time.Hour, false)
	q := generated.New(pool)

	// Both through the real registration path; the operator is created first.
	opOrg, opOwner, err := s.createOrgWithOwner(ctx, "Operator", "operator-"+id.New()[:6], id.New()+"@synthetic.invalid", "unused")
	if err != nil {
		t.Fatal(err)
	}
	tenantOrg, tenantOwner, err := s.createOrgWithOwner(ctx, "Tenant", "tenant-"+id.New()[:6], id.New()+"@synthetic.invalid", "unused")
	if err != nil {
		t.Fatal(err)
	}
	// Same created_at: the answer must not depend on a tie.
	if _, err := pool.Exec(ctx, `UPDATE orgs SET created_at = '2026-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	tenantAdmin := id.New()
	if _, err := q.CreateUser(ctx, generated.CreateUserParams{ID: tenantAdmin, OrgID: tenantOrg.ID, Email: tenantAdmin + "@synthetic.invalid", PasswordHash: "unused", Role: "admin"}); err != nil {
		t.Fatal(err)
	}

	cookie := func(uid, org string) *http.Cookie {
		t.Helper()
		w := httptest.NewRecorder()
		s.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.establishSession(r.Context(), uid, org)
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://flare.example/test-session", nil))
		for _, c := range w.Result().Cookies() {
			if c.Name == auth.SessionCookieName {
				return c
			}
		}
		t.Fatal("no session cookie")
		return nil
	}
	handler := s.Routes(fstest.MapFS{"index.html": {Data: []byte("test")}}, func(next http.Handler) http.Handler { return next })
	call := func(method, path, body string, c *http.Cookie) int {
		r := httptest.NewRequest(method, "https://flare.example"+path, strings.NewReader(body))
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		r.AddCookie(c)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	const sevenOff = `{"hot_retention_days":7,"cold_mode":"off","confirm_reduction":true}`

	if code := call("PUT", "/api/settings/retention", sevenOff, cookie(tenantAdmin, tenantOrg.ID)); code != http.StatusForbidden {
		t.Fatalf("tenant admin PUT before = %d, want 403", code)
	}
	if code := call("DELETE", "/api/org", "", cookie(opOwner.ID, opOrg.ID)); code != http.StatusConflict {
		t.Fatalf("operator owner DELETE /api/org with a tenant present = %d, want 409", code)
	}
	if _, err := q.GetOrgByID(ctx, opOrg.ID); err != nil {
		t.Fatalf("operator org gone: %v", err)
	}
	if code := call("PUT", "/api/settings/retention", sevenOff, cookie(tenantAdmin, tenantOrg.ID)); code != http.StatusForbidden {
		t.Fatalf("tenant admin PUT after = %d, want 403", code)
	}
	if p, err := s.RetentionPolicy(ctx); err != nil || p.HotDays != 30 {
		t.Fatalf("instance policy moved: %+v %v", p, err)
	}

	// Once the tenant has gone, the operator workspace is the last one and
	// may be erased like any other.
	if code := call("DELETE", "/api/org", "", cookie(tenantOwner.ID, tenantOrg.ID)); code != http.StatusNoContent {
		t.Fatalf("tenant owner DELETE /api/org = %d", code)
	}
	if code := call("DELETE", "/api/org", "", cookie(opOwner.ID, opOrg.ID)); code != http.StatusNoContent {
		t.Fatalf("last-workspace DELETE /api/org = %d", code)
	}
}

// Exactly one org holds the flag, and it is the first one created.
func TestCreateOrgClaimsTheOperatorFlagOnce(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	s := serverForDB(pool)
	var first string
	for i := 0; i < 3; i++ {
		org, _, err := s.createOrgWithOwner(ctx, "Org", "org-"+id.New()[:6], id.New()+"@synthetic.invalid", "unused")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = org.ID
		}
	}
	var ops []string
	rows, err := pool.Query(ctx, `SELECT id FROM orgs WHERE is_operator`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v string
		_ = rows.Scan(&v)
		ops = append(ops, v)
	}
	rows.Close()
	if len(ops) != 1 || ops[0] != first {
		t.Fatalf("operator orgs = %v, want exactly [%s]", ops, first)
	}
	if op, err := generated.New(pool).GetOperatorOrg(ctx); err != nil || op.ID != first {
		t.Fatalf("GetOperatorOrg = %v %v", op, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orgs SET is_operator = TRUE WHERE id <> $1`, first); err == nil {
		t.Fatal("a second operator org was accepted")
	}
}
