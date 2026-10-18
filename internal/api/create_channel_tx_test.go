package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

func TestChannelCreationOptions(t *testing.T) {
	for _, test := range []struct{ enabled, projects string }{
		{"null", "[]"}, {`"false"`, "[]"}, {"0", "[]"},
		{"false", "null"}, {"false", `{}`}, {"false", `[null]`},
		{"false", `[""]`}, {"false", `["bad project"]`}, {"false", `["é"]`},
		{"false", fmt.Sprintf(`[%q]`, strings.Repeat("a", 129))},
	} {
		if _, _, err := channelCreationOptions(json.RawMessage(test.enabled), json.RawMessage(test.projects)); err == nil {
			t.Fatalf("invalid creation options accepted: %s / %s", test.enabled, test.projects)
		}
	}
	enabled, projects, err := channelCreationOptions(nil, nil)
	if err != nil || !enabled || projects == nil || len(projects) != 0 {
		t.Fatalf("legacy defaults = %v %v %v", enabled, projects, err)
	}
	enabled, projects, err = channelCreationOptions(json.RawMessage(`false`), json.RawMessage(`["p-b","p-a","p-b"]`))
	if err != nil || enabled || !slices.Equal(projects, []string{"p-b", "p-a"}) {
		t.Fatalf("paused/deduplicated options = %v %v %v", enabled, projects, err)
	}
	ids := make([]string, maxChannelProjects+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("project-%d", i)
	}
	raw, _ := json.Marshal(ids)
	if _, _, err := channelCreationOptions(nil, raw); err == nil {
		t.Fatal("routing over cap admitted")
	}
	if got, err := normalizeChannelProjectIDs(slices.Repeat([]string{"same-project"}, 1000)); err != nil || !slices.Equal(got, []string{"same-project"}) {
		t.Fatalf("duplicate-heavy routing = %v %v", got, err)
	}
}

func postChannel(s *Server, org, body string) *httptest.ResponseRecorder {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, ctxOrgID, org)
	req := httptest.NewRequest(http.MethodPost, "/api/channels", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleCreateChannel(rec, req)
	return rec
}

func patchChannelBounded(s *Server, org, channel, body string) *httptest.ResponseRecorder {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", channel)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, ctxOrgID, org)
	req := httptest.NewRequest(http.MethodPatch, "/api/channels/"+channel, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleUpdateChannel(rec, req)
	return rec
}

func channelCreateOrg(t *testing.T, pool *pgxpool.Pool, projects int) (string, []string) {
	t.Helper()
	org, channel, ids := seedOrgProjectChannel(t, pool, projects)
	if _, err := pool.Exec(context.Background(), "DELETE FROM notification_channels WHERE id=$1 AND org_id=$2", channel, org); err != nil {
		t.Fatal(err)
	}
	return org, ids
}

func createdChannel(t *testing.T, rec *httptest.ResponseRecorder) channelResponse {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response channelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func storedChannelCount(t *testing.T, pool *pgxpool.Pool, org string) int64 {
	t.Helper()
	n, err := generated.New(pool).CountNotificationChannelsByOrg(context.Background(), org)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func seedExtraChannels(t *testing.T, pool *pgxpool.Pool, org string, n int, enabled bool) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for range n {
		ch, err := generated.New(pool).CreateNotificationChannel(context.Background(), generated.CreateNotificationChannelParams{
			ID: id.New(), OrgID: org, Type: "log", Config: json.RawMessage(`{}`), Enabled: enabled,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ch.ID)
	}
	return ids
}

func waitForDatabaseLockWaiters(t *testing.T, pool *pgxpool.Pool, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	for {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
WHERE datname=current_database() AND state='active' AND wait_event_type='Lock' AND pid<>pg_backend_pid()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= count {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("requests did not reach the controlled database lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestCreateChannelTransaction(t *testing.T) {
	// One new disposable database; the configured shared fixture is untouched.
	pool := newTestDB(t)
	s := serverForDB(pool)

	t.Run("scoped paused channel cannot receive alerts", func(t *testing.T) {
		org, projects := channelCreateOrg(t, pool, 2)
		body := fmt.Sprintf(`{"type":"log","enabled":false,"project_ids":[%q,%q]}`, projects[0], projects[0])
		ch := createdChannel(t, postChannel(s, org, body))
		if ch.Enabled || !slices.Equal(ch.ProjectIDs, projects[:1]) || !slices.Equal(routingRows(t, pool, ch.ID), projects[:1]) {
			t.Fatalf("paused channel/routing mismatch: %+v", ch)
		}
		for _, project := range projects {
			rows, err := s.q.ListEnabledChannelsForProject(context.Background(), generated.ListEnabledChannelsForProjectParams{OrgID: org, ProjectID: project})
			if err != nil || len(rows) != 0 {
				t.Fatalf("paused channel is eligible for dispatch: %v %v", rows, err)
			}
		}
		if rec := patchChannel(s, org, ch.ID, `{"enabled":true}`); rec.Code != http.StatusOK {
			t.Fatalf("enable status=%d", rec.Code)
		}
		for index, project := range projects {
			rows, err := s.q.ListEnabledChannelsForProject(context.Background(), generated.ListEnabledChannelsForProjectParams{OrgID: org, ProjectID: project})
			if err != nil || len(rows) != 1-index {
				t.Fatalf("enabled routing crossed project: %d rows=%d err=%v", index, len(rows), err)
			}
		}
	})

	t.Run("omitted legacy options remain enabled all projects", func(t *testing.T) {
		org, projects := channelCreateOrg(t, pool, 2)
		ch := createdChannel(t, postChannel(s, org, `{"type":"log"}`))
		if !ch.Enabled || ch.ProjectIDs == nil || len(ch.ProjectIDs) != 0 {
			t.Fatalf("legacy response changed: %+v", ch)
		}
		for _, project := range projects {
			rows, err := s.q.ListEnabledChannelsForProject(context.Background(), generated.ListEnabledChannelsForProjectParams{OrgID: org, ProjectID: project})
			if err != nil || len(rows) != 1 || rows[0].ID != ch.ID {
				t.Fatalf("legacy all-project routing changed: %v %v", rows, err)
			}
		}
	})

	t.Run("foreign unknown and malformed routing persist nothing", func(t *testing.T) {
		org, projects := channelCreateOrg(t, pool, 1)
		_, foreign := channelCreateOrg(t, pool, 1)
		for _, body := range []string{
			fmt.Sprintf(`{"type":"log","project_ids":[%q,%q]}`, projects[0], foreign[0]),
			fmt.Sprintf(`{"type":"log","project_ids":[%q,"unknown-project"]}`, projects[0]),
			`{"type":"log","enabled":null}`, `{"type":"log","project_ids":null}`,
			`{"type":"log","project_ids":[""]}`,
		} {
			if rec := postChannel(s, org, body); rec.Code != http.StatusBadRequest {
				t.Fatalf("invalid creation status=%d body=%s", rec.Code, rec.Body.String())
			}
			if n := storedChannelCount(t, pool, org); n != 0 {
				t.Fatalf("failed validation left %d channels", n)
			}
		}
	})

	t.Run("routing insertion failure rolls back channel and earlier route", func(t *testing.T) {
		org, projects := channelCreateOrg(t, pool, 2)
		constraint := fmt.Sprintf(`ALTER TABLE channel_projects ADD CONSTRAINT create_channel_refuse CHECK(project_id <> '%s') NOT VALID`, projects[1])
		if _, err := pool.Exec(context.Background(), constraint); err != nil {
			t.Fatal(err)
		}
		defer pool.Exec(context.Background(), "ALTER TABLE channel_projects DROP CONSTRAINT create_channel_refuse")
		body := fmt.Sprintf(`{"type":"log","project_ids":[%q,%q]}`, projects[0], projects[1])
		if rec := postChannel(s, org, body); rec.Code != http.StatusInternalServerError {
			t.Fatalf("injected route failure status=%d body=%s", rec.Code, rec.Body.String())
		}
		if n := storedChannelCount(t, pool, org); n != 0 {
			t.Fatalf("failed routing left %d channels", n)
		}
	})

	t.Run("channel never visible without its complete routing", func(t *testing.T) {
		org, projects := channelCreateOrg(t, pool, 2)
		ctx := context.Background()
		blocker, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Release()
		if _, err := blocker.Exec(ctx, "SELECT pg_advisory_lock(8372661)"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			// Unblock the request before DROP waits for its table lock, including
			// when an assertion fails before the normal explicit unlock below.
			blocker.Exec(ctx, "SELECT pg_advisory_unlock(8372661)")
			pool.Exec(ctx, "DROP TRIGGER IF EXISTS pause_channel_route ON channel_projects; DROP FUNCTION IF EXISTS pause_channel_route()")
		}()
		if _, err := pool.Exec(ctx, `CREATE FUNCTION pause_channel_route() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN PERFORM pg_advisory_xact_lock(8372661); RETURN NEW; END $$;
CREATE TRIGGER pause_channel_route BEFORE INSERT ON channel_projects FOR EACH ROW EXECUTE FUNCTION pause_channel_route()`); err != nil {
			t.Fatal(err)
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- postChannel(s, org, fmt.Sprintf(`{"type":"log","project_ids":[%q]}`, projects[0])) }()
		waitForDatabaseLockWaiters(t, pool, 1)
		if n := storedChannelCount(t, pool, org); n != 0 {
			t.Fatalf("uncommitted channel visible before route insert: %d", n)
		}
		for _, project := range projects {
			rows, err := s.q.ListEnabledChannelsForProject(ctx, generated.ListEnabledChannelsForProjectParams{OrgID: org, ProjectID: project})
			if err != nil || len(rows) != 0 {
				t.Fatalf("dispatch saw a partially created channel: %v %v", rows, err)
			}
		}
		if _, err := blocker.Exec(ctx, "SELECT pg_advisory_unlock(8372661)"); err != nil {
			t.Fatal(err)
		}
		createdChannel(t, <-done)
	})

	t.Run("paused storage cap and concurrent admission", func(t *testing.T) {
		org, _ := channelCreateOrg(t, pool, 0)
		seedExtraChannels(t, pool, org, maxChannelsPerOrg-1, false)
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(context.Background(), "SELECT id FROM orgs WHERE id=$1 FOR UPDATE", org); err != nil {
			t.Fatal(err)
		}
		done := make(chan *httptest.ResponseRecorder, 2)
		for range 2 {
			go func() { done <- postChannel(s, org, `{"type":"log","enabled":false,"project_ids":[]}`) }()
		}
		waitForDatabaseLockWaiters(t, pool, 2)
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		codes := []int{(<-done).Code, (<-done).Code}
		slices.Sort(codes)
		if !slices.Equal(codes, []int{http.StatusCreated, http.StatusConflict}) || storedChannelCount(t, pool, org) != maxChannelsPerOrg {
			t.Fatalf("concurrent cap statuses=%v count=%d", codes, storedChannelCount(t, pool, org))
		}
		if rec := postChannel(s, org, `{"type":"log","enabled":false}`); rec.Code != http.StatusConflict {
			t.Fatalf("paused channel bypassed storage cap: %d", rec.Code)
		}
	})

	t.Run("concurrent routing and enable patches serialize without deadlock or route union", func(t *testing.T) {
		org, projects := channelCreateOrg(t, pool, 3)
		ch := createdChannel(t, postChannel(s, org, fmt.Sprintf(`{"type":"log","enabled":false,"project_ids":[%q]}`, projects[0])))
		ctx := context.Background()
		blocker, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Release()
		if _, err := blocker.Exec(ctx, "SELECT pg_advisory_lock(8372662)"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			// Release blocked requests before taking the table lock for cleanup,
			// including on a failed assertion. Request deadlines bound their exit.
			blocker.Exec(ctx, "SELECT pg_advisory_unlock(8372662)")
			pool.Exec(ctx, "DROP TRIGGER IF EXISTS pause_channel_patch_route ON channel_projects; DROP FUNCTION IF EXISTS pause_channel_patch_route()")
		}()
		// Pause only the first PATCH, after its routing DELETE and before its
		// INSERT/FK check. The later enabling PATCH must wait at the channel
		// lock, rather than hold a conflicting lock while waiting on old routes.
		if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION pause_channel_patch_route() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.project_id = '%s' THEN PERFORM pg_advisory_xact_lock(8372662); END IF; RETURN NEW; END $$;
CREATE TRIGGER pause_channel_patch_route BEFORE INSERT ON channel_projects FOR EACH ROW EXECUTE FUNCTION pause_channel_patch_route()`, projects[1])); err != nil {
			t.Fatal(err)
		}
		routingDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			routingDone <- patchChannelBounded(s, org, ch.ID, fmt.Sprintf(`{"project_ids":[%q]}`, projects[1]))
		}()
		waitForDatabaseLockWaiters(t, pool, 1)
		enableDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			enableDone <- patchChannelBounded(s, org, ch.ID, fmt.Sprintf(`{"enabled":true,"project_ids":[%q]}`, projects[2]))
		}()
		waitForDatabaseLockWaiters(t, pool, 2)
		if _, err := blocker.Exec(ctx, "SELECT pg_advisory_unlock(8372662)"); err != nil {
			t.Fatal(err)
		}
		for _, rec := range []*httptest.ResponseRecorder{<-routingDone, <-enableDone} {
			if rec.Code != http.StatusOK {
				t.Fatalf("concurrent PATCH status=%d body=%s", rec.Code, rec.Body.String())
			}
		}
		if got := routingRows(t, pool, ch.ID); !slices.Equal(got, projects[2:]) {
			t.Fatalf("later PATCH did not replace routing exactly: %v", got)
		}
		if !channelEnabled(t, pool, org, ch.ID) {
			t.Fatal("later PATCH did not enable the channel")
		}
	})

	t.Run("legacy enable cap and idempotent recovery", func(t *testing.T) {
		org, _ := channelCreateOrg(t, pool, 0)
		enabled := seedExtraChannels(t, pool, org, maxChannelsPerOrg, true)
		paused := seedExtraChannels(t, pool, org, 1, false)[0]
		if rec := patchChannel(s, org, paused, `{"enabled":true}`); rec.Code != http.StatusConflict || channelEnabled(t, pool, org, paused) {
			t.Fatalf("legacy enable exceeded cap: %d", rec.Code)
		}
		if rec := patchChannel(s, org, enabled[0], `{"enabled":true}`); rec.Code != http.StatusOK {
			t.Fatalf("idempotent enabled patch refused: %d", rec.Code)
		}
		if rec := patchChannel(s, org, enabled[0], `{"enabled":false}`); rec.Code != http.StatusOK {
			t.Fatalf("pause refused at cap: %d", rec.Code)
		}
		if rec := patchChannel(s, org, paused, `{"enabled":true}`); rec.Code != http.StatusOK {
			t.Fatalf("enable refused after pause freed slot: %d", rec.Code)
		}
	})
}
