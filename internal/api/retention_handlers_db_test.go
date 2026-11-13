package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/retention"
)

// The retention routes are instance-wide destruction controls: they must sit in
// an admin group (no API key can reach admin), never in the member group.
func TestRetentionRoutesAreAdminGated(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	memberStart := strings.Index(body, `s.requireRole("member")`)
	rest := body[memberStart+1:]
	next := regexp.MustCompile(`s\.requireRole\("(admin|owner)"\)`).FindStringIndex(rest)
	if memberStart < 0 || next == nil {
		t.Fatal("router shape changed; update this test")
	}
	memberBlock, adminAndBelow := rest[:next[0]], rest[next[0]:]
	for _, h := range []string{"handleGetRetention", "handleSetRetention", "handleResetRetention"} {
		if strings.Contains(memberBlock, h) {
			t.Errorf("%s is in the member group", h)
		}
		if !strings.Contains(adminAndBelow, h) {
			t.Errorf("%s is not routed behind an admin gate", h)
		}
	}
}

func TestRetentionSettingsAPI(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	q := generated.New(pool)
	operator, _, _ := seedOrgProjectChannel(t, pool, 0)
	tenant, _, _ := seedOrgProjectChannel(t, pool, 0)
	// The seeded orgs bypass CreateOrg, so flag the operator workspace by hand.
	if _, err := pool.Exec(ctx, `UPDATE orgs SET is_operator = TRUE WHERE id = $1`, operator); err != nil {
		t.Fatal(err)
	}

	// One dated partition so the hot inventory has something to report.
	invDay := time.Now().UTC().AddDate(0, 0, -3)
	if _, err := pool.Exec(ctx, "CREATE TABLE events_"+invDay.Format("2006_01_02")+" PARTITION OF events FOR VALUES FROM ('"+
		invDay.Format("2006-01-02")+"') TO ('"+invDay.AddDate(0, 0, 1).Format("2006-01-02")+"')"); err != nil {
		t.Fatal(err)
	}

	s := serverForDB(pool)
	s.cfg.BaseURL = "https://flare.example"
	s.cfg.RetentionDays = 30
	s.cfg.ColdRetentionMode = retention.ColdForever
	s.sessions = auth.NewSessionManager(time.Hour, time.Hour, false)

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
		for _, c := range w.Result().Cookies() {
			if c.Name == auth.SessionCookieName {
				return c
			}
		}
		t.Fatal("session cookie not created")
		return nil
	}
	admin, owner := session(operator, "admin"), session(operator, "owner")
	member, viewer := session(operator, "member"), session(operator, "viewer")
	tenantOwner := session(tenant, "owner")

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
	call := func(method, path, body string, cookie *http.Cookie, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://flare.example"+path, strings.NewReader(body))
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
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
	decode := func(w *httptest.ResponseRecorder) retentionResponse {
		t.Helper()
		var v retentionResponse
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
		return v
	}
	const path = "/api/settings/retention"
	const sevenOff = `{"hot_retention_days":7,"cold_mode":"off"}`

	// Unauthenticated, viewer, member, and an owner of ANOTHER workspace are
	// all refused, for reads and writes.
	if w := call("GET", path, "", nil, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d", w.Code)
	}
	for name, c := range map[string]*http.Cookie{"viewer": viewer, "member": member, "tenant owner": tenantOwner} {
		if w := call("GET", path, "", c, false); w.Code != http.StatusForbidden {
			t.Errorf("%s GET = %d, want 403", name, w.Code)
		}
		if w := call("PUT", path, sevenOff+``, c, true); w.Code != http.StatusForbidden {
			t.Errorf("%s PUT = %d, want 403", name, w.Code)
		}
		if w := call("DELETE", path, "", c, true); w.Code != http.StatusForbidden {
			t.Errorf("%s DELETE = %d, want 403", name, w.Code)
		}
	}
	// CSRF is enforced on writes like every other settings route.
	if w := call("PUT", path, `{"hot_retention_days":7,"cold_mode":"off","confirm_reduction":true}`, admin, false); w.Code != http.StatusForbidden {
		t.Fatalf("PUT without CSRF = %d, want 403", w.Code)
	}

	// Nothing stored: the env default is effective and labelled as such.
	w := call("GET", path, "", admin, false)
	if w.Code != http.StatusOK {
		t.Fatalf("admin GET = %d %s", w.Code, w.Body.String())
	}
	v := decode(w)
	if v.Source != retention.SourceEnv || v.Effective.HotDays != 30 || v.Effective.ColdMode != retention.ColdForever || v.Stored != nil {
		t.Fatalf("env default view = %+v", v)
	}
	if v.Bounds.HotMin != 7 || v.Bounds.HotMax != 365 || v.Bounds.ColdMax != 3650 {
		t.Fatalf("bounds = %+v", v.Bounds)
	}
	if v.ColdTier.Kind != "unavailable" || len(v.Hot) != 1 || v.Hot[0].Day != invDay.Format("2006-01-02") || v.Hot[0].Bytes <= 0 {
		t.Fatalf("cold tier / hot inventory = %+v / %d days", v.ColdTier, len(v.Hot))
	}

	// Out-of-bounds and malformed bodies are rejected.
	for _, body := range []string{
		`{"hot_retention_days":6,"cold_mode":"forever","confirm_reduction":true}`,
		`{"hot_retention_days":366,"cold_mode":"forever"}`,
		`{"hot_retention_days":30,"cold_mode":"days","cold_retention_days":29,"confirm_reduction":true}`,
		`{"hot_retention_days":30,"cold_mode":"days","cold_retention_days":3651}`,
		`{"hot_retention_days":30,"cold_mode":"off","cold_retention_days":30,"confirm_reduction":true}`,
		`{"hot_retention_days":30,"cold_mode":"purge"}`,
		`{"hot_retention_days":30}`,
		`{"hot_retention_days":30,"cold_mode":"forever","extra":1}`,
	} {
		if w := call("PUT", path, body, admin, true); w.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", body, w.Code)
		}
	}

	// Lowering without confirming is refused, and nothing is stored.
	if w := call("PUT", path, sevenOff, admin, true); w.Code != http.StatusConflict {
		t.Fatalf("unconfirmed reduction = %d, want 409", w.Code)
	}
	if _, err := q.GetRetentionSettings(ctx); err == nil {
		t.Fatal("an unconfirmed reduction was stored")
	}

	// Raising needs no confirmation.
	w = call("PUT", path, `{"hot_retention_days":60,"cold_mode":"forever"}`, admin, true)
	if w.Code != http.StatusOK {
		t.Fatalf("raise = %d %s", w.Code, w.Body.String())
	}
	if v := decode(w); v.Source != retention.SourceStored || v.Effective.HotDays != 60 || v.UpdatedBy == "" || v.UpdatedAt == nil {
		t.Fatalf("after raise = %+v", v)
	}

	// The operator's request: 7 days, no archive, confirmed. The partition
	// manager's PolicyFunc sees it immediately.
	w = call("PUT", path, `{"hot_retention_days":7,"cold_mode":"off","confirm_reduction":true}`, owner, true)
	if w.Code != http.StatusOK {
		t.Fatalf("confirmed reduction = %d %s", w.Code, w.Body.String())
	}
	if p, err := s.RetentionPolicy(ctx); err != nil || p != (retention.Policy{HotDays: 7, ColdMode: retention.ColdOff}) {
		t.Fatalf("partition manager would see %+v %v", p, err)
	}

	// Archive with a day count. Coming from "off" this prunes the archive days
	// "off" left on disk, so it is a reduction however large the count is.
	if w := call("PUT", path, `{"hot_retention_days":7,"cold_mode":"days","cold_retention_days":90}`, admin, true); w.Code != http.StatusConflict {
		t.Fatalf("off -> days without confirm = %d, want 409", w.Code)
	}
	w = call("PUT", path, `{"hot_retention_days":7,"cold_mode":"days","cold_retention_days":90,"confirm_reduction":true}`, admin, true)
	if w.Code != http.StatusOK || decode(w).Effective.ColdDays != 90 {
		t.Fatalf("days mode = %d %s", w.Code, w.Body.String())
	}

	// Reset to env: env keeps more here (30d, forever), so no confirmation.
	w = call("DELETE", path, "", admin, true)
	if w.Code != http.StatusOK || decode(w).Source != retention.SourceEnv {
		t.Fatalf("reset = %d %s", w.Code, w.Body.String())
	}
	// Reset that would REDUCE (stored 60d > env 30d) needs confirmation.
	if w := call("PUT", path, `{"hot_retention_days":60,"cold_mode":"forever"}`, admin, true); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if w := call("DELETE", path, "", admin, true); w.Code != http.StatusConflict {
		t.Fatalf("unconfirmed reducing reset = %d, want 409", w.Code)
	}
	if w := call("DELETE", path+"?confirm_reduction=true", "", admin, true); w.Code != http.StatusOK {
		t.Fatalf("confirmed reset = %d", w.Code)
	}

	// Every change is in the operator workspace's audit log, with before/after.
	rows, err := q.ListAuditLog(ctx, operator)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, r := range rows {
		if strings.HasPrefix(r.Action, "retention.") {
			actions = append(actions, r.Action+": "+r.Target)
		}
	}
	joined := strings.Join(actions, "\n")
	for _, want := range []string{
		"retention.update: hot 60d, archive forever -> hot 7d, no archive",
		"retention.update: hot 7d, no archive -> hot 7d, archive 90d",
		"retention.reset: hot 7d, archive 90d -> env default hot 30d, archive forever",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit log missing %q in:\n%s", want, joined)
		}
	}
	if len(actions) != 6 {
		t.Errorf("want 6 retention audit rows (4 updates, 2 resets), got %d:\n%s", len(actions), joined)
	}
	if other, _ := q.ListAuditLog(ctx, tenant); len(other) != 0 {
		t.Fatal("retention changes leaked into another workspace's audit log")
	}

	// Reset from a stored "off" to an env that prunes the archive is a
	// reduction too: the env would delete the days "off" kept.
	s.cfg.ColdRetentionMode, s.cfg.ColdRetentionDays = retention.ColdDays, 30
	if w := call("PUT", path, `{"hot_retention_days":30,"cold_mode":"off","confirm_reduction":true}`, admin, true); w.Code != http.StatusOK {
		t.Fatalf("store off = %d %s", w.Code, w.Body.String())
	}
	if w := call("DELETE", path, "", admin, true); w.Code != http.StatusConflict {
		t.Fatalf("reset off -> env days without confirm = %d, want 409", w.Code)
	}
	if w := call("DELETE", path+"?confirm_reduction=true", "", admin, true); w.Code != http.StatusOK {
		t.Fatalf("confirmed reset from off = %d", w.Code)
	}
}

// The DB constraint is the last line of defence if a future code path skips
// Validate.
func TestRetentionSettingsTableRejectsUnsafeRows(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	for _, row := range []struct {
		hot  int
		mode string
		days any
	}{
		{6, "forever", nil},
		{30, "days", 29},
		{30, "days", nil},
		{30, "off", 30},
		{30, "wipe", nil},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO retention_settings (hot_retention_days, cold_mode, cold_retention_days) VALUES ($1,$2,$3)`, row.hot, row.mode, row.days); err == nil {
			t.Errorf("row %+v accepted", row)
			_, _ = pool.Exec(ctx, `DELETE FROM retention_settings`)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO retention_settings (hot_retention_days, cold_mode) VALUES (7,'off')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO retention_settings (singleton, hot_retention_days, cold_mode) VALUES (FALSE, 7,'off')`); err == nil {
		t.Fatal("a second policy row was accepted")
	}
}
