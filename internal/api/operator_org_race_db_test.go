package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/id"
)

// Org creation and org deletion decide who the operator workspace is, so they
// must be serialised (fix audit B). A registration that committed inside an
// operator delete saw the operator still present, became a tenant, and left
// the instance with orgs but no operator; the next registrant then got it.
// Here the test holds the shared lock, as an in-flight delete does, and the
// registration has to wait for it.
func TestRegistrationWaitsForAnInFlightOperatorDelete(t *testing.T) {
	pool := newTestDB(t)
	s := serverForDB(pool)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('flare.operator_org'))`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := s.createOrgWithOwner(ctx, "Racer", "racer-"+id.New()[:6], id.New()+"@synthetic.invalid", "unused")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("registration committed while the operator lock was held (err=%v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The delete side takes the same lock before its operator check.
func TestOrgDeleteTakesTheOperatorLock(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	s := serverForDB(pool)
	s.cfg.BaseURL = "https://flare.example"
	s.sessions = auth.NewSessionManager(time.Hour, time.Hour, false)
	org, owner, err := s.createOrgWithOwner(ctx, "Solo", "solo-"+id.New()[:6], id.New()+"@synthetic.invalid", "unused")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.establishSession(r.Context(), owner.ID, org.ID)
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://flare.example/test-session", nil))
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			cookie = c
		}
	}
	handler := s.Routes(fstest.MapFS{"index.html": {Data: []byte("test")}}, func(next http.Handler) http.Handler { return next })

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('flare.operator_org'))`); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		r := httptest.NewRequest("DELETE", "https://flare.example/api/org", nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		done <- w.Code
	}()
	select {
	case code := <-done:
		t.Fatalf("DELETE /api/org finished (%d) while the operator lock was held", code)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != http.StatusNoContent {
		t.Fatalf("last-workspace delete = %d", code)
	}
}

// Only an EMPTY instance hands out the operator flag. Orgs with no operator
// (an operator deleted on an old binary, a hand edit) stay a refusal for a
// human to repair, never a prize for the next registrant, and boot says so.
func TestOperatorFlagIsOnlyClaimedOnAnEmptyInstance(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	s := serverForDB(pool)
	first, _, err := s.createOrgWithOwner(ctx, "First", "first-"+id.New()[:6], id.New()+"@synthetic.invalid", "unused")
	if err != nil {
		t.Fatal(err)
	}
	if !first.IsOperator {
		t.Fatal("the first org on an empty instance must be the operator")
	}
	if missing, err := s.CheckOperatorOrg(ctx); err != nil || missing {
		t.Fatalf("boot check with an operator = %v %v", missing, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orgs SET is_operator = FALSE`); err != nil {
		t.Fatal(err)
	}
	if missing, err := s.CheckOperatorOrg(ctx); err != nil || !missing {
		t.Fatalf("boot check with orgs but no operator = %v %v, want missing", missing, err)
	}
	next, _, err := s.createOrgWithOwner(ctx, "Stranger", "stranger-"+id.New()[:6], id.New()+"@synthetic.invalid", "unused")
	if err != nil {
		t.Fatal(err)
	}
	if next.IsOperator {
		t.Fatal("a registrant on a non-empty instance with no operator was made the operator")
	}
}
