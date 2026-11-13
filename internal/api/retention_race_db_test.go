package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/retention"
)

// retentionHarness is an operator-workspace admin session on the real routes
// (CSRF gate passed through, as the CSRF case is covered in
// TestRetentionSettingsAPI). Env default: 30 days, archive forever.
func retentionHarness(t *testing.T) (*Server, *pgxpool.Pool, string, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	pool := newTestDB(t)
	ctx := context.Background()
	q := generated.New(pool)
	operator, _, _ := seedOrgProjectChannel(t, pool, 0)
	if _, err := pool.Exec(ctx, `UPDATE orgs SET is_operator = TRUE WHERE id = $1`, operator); err != nil {
		t.Fatal(err)
	}
	s := serverForDB(pool)
	s.cfg.BaseURL = "https://flare.example"
	s.cfg.RetentionDays = 30
	s.cfg.ColdRetentionMode = retention.ColdForever
	s.sessions = auth.NewSessionManager(time.Hour, time.Hour, false)
	uid := id.New()
	if _, err := q.CreateUser(ctx, generated.CreateUserParams{ID: uid, OrgID: operator, Email: uid + "@synthetic.invalid", PasswordHash: "unused-test-session", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.establishSession(r.Context(), uid, operator)
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://flare.example/test-session", nil))
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("session cookie not created")
	}
	pass := func(next http.Handler) http.Handler { return next }
	handler := s.Routes(fstest.MapFS{"index.html": {Data: []byte("test")}}, pass)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://flare.example"+path, strings.NewReader(body))
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	return s, pool, operator, call
}

func retentionAuditRows(t *testing.T, pool *pgxpool.Pool, org string) []string {
	t.Helper()
	rows, err := generated.New(pool).ListAuditLog(context.Background(), org)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		if strings.HasPrefix(r.Action, "retention.") {
			out = append(out, r.Action+": "+r.Target)
		}
	}
	return out
}

// Admin A saves 60 days (a raise from the 30 A loaded) while admin B's save
// of 365 days is mid-transaction. A must not land 365 -> 60 unconfirmed, and
// no audit row may describe a write that did not happen.
func TestRetentionConcurrentSaveCannotSlipAnUnconfirmedReduction(t *testing.T) {
	for _, stored := range []bool{true, false} {
		name := "nothing stored yet"
		if stored {
			name = "row already stored"
		}
		t.Run(name, func(t *testing.T) {
			s, pool, operator, call := retentionHarness(t)
			ctx := context.Background()
			if stored {
				if w := call("PUT", "/api/settings/retention", `{"hot_retention_days":30,"cold_mode":"forever"}`); w.Code != http.StatusOK {
					t.Fatal(w.Body.String())
				}
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			// B, the way the write path does it: take the lock, then write 365.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('flare.retention_settings'))`); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO retention_settings (singleton, hot_retention_days, cold_mode, updated_at)
				VALUES (TRUE, 365, 'forever', now())
				ON CONFLICT (singleton) DO UPDATE SET hot_retention_days = 365, cold_mode = 'forever', cold_retention_days = NULL, updated_at = now()`); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			var codeA int
			wg.Add(1)
			go func() {
				defer wg.Done()
				codeA = call("PUT", "/api/settings/retention", `{"hot_retention_days":60,"cold_mode":"forever"}`).Code
			}()
			time.Sleep(400 * time.Millisecond) // A is in flight, behind B
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			p, err := s.RetentionPolicy(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if p.HotDays != 365 || codeA != http.StatusConflict {
				t.Fatalf("A = %d, stored hot = %d: an unconfirmed 365 -> 60 reduction landed (want 409, 365 kept)", codeA, p.HotDays)
			}
			for _, row := range retentionAuditRows(t, pool, operator) {
				if strings.Contains(row, "-> hot 60d") {
					t.Fatalf("audit row for a refused write: %s", row)
				}
			}
		})
	}
}

// The browser's view is stale: another admin raised retention after it
// loaded. A save carrying the stale expected state is refused with 409 and
// the fresh view, even with confirm_reduction set; a save carrying the fresh
// state goes through. Reset behaves the same.
func TestRetentionStaleExpectedIsRefusedWithTheFreshView(t *testing.T) {
	_, pool, operator, call := retentionHarness(t)
	view := func() retentionResponse {
		t.Helper()
		var v retentionResponse
		if err := json.Unmarshal(call("GET", "/api/settings/retention", "").Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	expected := func(v retentionResponse) string {
		b, _ := json.Marshal(retentionExpected{Effective: v.Effective, UpdatedAt: v.UpdatedAt})
		return string(b)
	}
	stale := expected(view())
	if w := call("PUT", "/api/settings/retention", `{"hot_retention_days":90,"cold_mode":"forever"}`); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}

	w := call("PUT", "/api/settings/retention", `{"hot_retention_days":60,"cold_mode":"forever","confirm_reduction":true,"expected":`+stale+`}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale expected = %d %s, want 409", w.Code, w.Body.String())
	}
	var conflict retentionConflict
	if err := json.Unmarshal(w.Body.Bytes(), &conflict); err != nil || conflict.Current == nil || conflict.Current.Effective.HotDays != 90 {
		t.Fatalf("409 must carry the fresh view: %s (%v)", w.Body.String(), err)
	}
	fresh := expected(*conflict.Current)
	if w := call("PUT", "/api/settings/retention", `{"hot_retention_days":60,"cold_mode":"forever","confirm_reduction":true,"expected":`+fresh+`}`); w.Code != http.StatusOK {
		t.Fatalf("fresh expected = %d %s", w.Code, w.Body.String())
	}

	if w := call("DELETE", "/api/settings/retention", `{"confirm_reduction":true,"expected":`+fresh+`}`); w.Code != http.StatusConflict {
		t.Fatalf("reset with stale expected = %d, want 409", w.Code)
	}
	if w := call("DELETE", "/api/settings/retention", `{"confirm_reduction":true,"expected":`+expected(view())+`}`); w.Code != http.StatusOK {
		t.Fatalf("reset with fresh expected = %d %s", w.Code, w.Body.String())
	}

	got := strings.Join(retentionAuditRows(t, pool, operator), "\n")
	for _, want := range []string{
		"retention.update: hot 30d, archive forever -> hot 90d, archive forever",
		"retention.update: hot 90d, archive forever -> hot 60d, archive forever",
		"retention.reset: hot 60d, archive forever -> env default hot 30d, archive forever",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("audit missing %q in:\n%s", want, got)
		}
	}
}

// The audit row is part of the change: if it cannot be written, the policy
// does not change either, and the caller is told.
func TestRetentionChangeRollsBackWhenTheAuditInsertFails(t *testing.T) {
	s, pool, _, call := retentionHarness(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_log ADD CONSTRAINT no_retention_rows CHECK (action NOT LIKE 'retention.%')`); err != nil {
		t.Fatal(err)
	}
	if w := call("PUT", "/api/settings/retention", `{"hot_retention_days":60,"cold_mode":"forever"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("save with a failing audit insert = %d, want 500", w.Code)
	}
	if p, src, _, err := s.effectiveRetention(ctx); err != nil || src != retention.SourceEnv || p.HotDays != 30 {
		t.Fatalf("policy changed without an audit row: %+v %s %v", p, src, err)
	}
}
