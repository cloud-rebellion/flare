package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/automationdelivery"
	"github.com/bright-interaction/flare/internal/automationoutbox"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func enqueueDeliveryFixture(t *testing.T, pool *pgxpool.Pool, org, project, channel, event string) automationoutbox.Notice {
	t.Helper()
	ctx := context.Background()
	store := automationoutbox.New(pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	n := automationoutbox.Notice{ID: id.New(), OrgID: org, ProjectID: project, ChannelID: channel, IssueID: "issue-1", Event: event, EventCount: 3, ObservedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if _, err := store.EnqueueTx(ctx, tx, n); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	batch, err := store.ClaimDue(ctx, automationoutbox.BatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range batch {
		if row.ID == n.ID {
			return row
		}
	}
	t.Fatal("fixture notice not claimed")
	return n
}

func TestAutomationDeliveryListScopedKeysetAndIssueFilter(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	q := generated.New(pool)
	org, channel, projects := seedOrgProjectChannel(t, pool, 2)
	foreignOrg, foreignChannel, foreignProjects := seedOrgProjectChannel(t, pool, 1)
	at := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	seed := func(prefix, org, project, channel string, count int) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO automation_alert_outbox
            (id,org_id,project_id,channel_id,issue_id,event,event_count,observed_at,created_at)
            SELECT $1||lpad(i::text,3,'0'),$2,$3,$4,CASE WHEN i%2=0 THEN 'issue-a' ELSE 'issue-b' END,
                   'new_issue',3,$5,$5 FROM generate_series(0,$6::int-1) AS i`, prefix, org, project, channel, at, count)
		if err != nil {
			t.Fatal(err)
		}
	}
	seed("page-main-", org, projects[0], channel, 130)
	seed("page-foreign-", foreignOrg, foreignProjects[0], foreignChannel, 1)
	// Both query shapes need an ordered index through the id tie-breaker:
	// a large equal-created_at group must not require a group sort to page.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan=off`); err != nil {
		t.Fatal(err)
	}
	for _, planCase := range []struct{ filter, index string }{{"", "automation_alert_outbox_delivery_page_idx"}, {" AND issue_id='issue-a'", "automation_alert_outbox_issue_page_idx"}} {
		var plan string
		if err := tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) SELECT id FROM automation_alert_outbox WHERE org_id=$1 AND project_id=$2`+planCase.filter+` AND (created_at,id)<($3::timestamptz,$4::text) ORDER BY created_at DESC,id DESC LIMIT 101`, org, projects[0], at, "page-main-129").Scan(&plan); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan, planCase.index) || strings.Contains(plan, `"Node Type": "Sort"`) || strings.Contains(plan, `"Node Type": "Incremental Sort"`) {
			t.Fatalf("page query lacks complete ordered index: %s", plan)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	mint := func(org string) string {
		t.Helper()
		token := "synthetic-list-viewer-" + id.New()
		if _, err := q.CreateAPIKey(ctx, generated.CreateAPIKeyParams{ID: id.New(), OrgID: org, Name: "delivery list viewer", KeyHash: auth.HashAPIKey(token), KeyPrefix: "synthetic", Role: "viewer"}); err != nil {
			t.Fatal(err)
		}
		return token
	}
	viewer, foreignViewer := mint(org), mint(foreignOrg)
	s := serverForDB(pool)
	s.sessions = auth.NewSessionManager(time.Hour, time.Hour, false)
	handler := s.Routes(fstest.MapFS{"index.html": {Data: []byte("test")}}, func(next http.Handler) http.Handler { return next })
	request := func(project, token string, params url.Values) (*httptest.ResponseRecorder, automationoutbox.DeliveryPage) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/projects/"+project+"/automation-deliveries?"+params.Encode(), nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		var page automationoutbox.DeliveryPage
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
		}
		return w, page
	}
	w, first := request(projects[0], viewer, nil)
	if w.Code != 200 || len(first.Items) != 25 || first.NextCursor == "" || first.Items[0].NoticeID != "page-main-129" || first.Items[24].NoticeID != "page-main-105" {
		t.Fatalf("default page: %d %+v", w.Code, first)
	}
	for _, forbidden := range []string{"org_id", "project_id", "lease_token", "signing_secret", "provider", "workflow_succeeded", "\"url\""} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatalf("listing leaked %q", forbidden)
		}
	}
	// Newer notices arriving after the first page must not duplicate or shift
	// the older sequence. Equal timestamps exercise the id tie-breaker.
	if _, err := pool.Exec(ctx, `INSERT INTO automation_alert_outbox (id,org_id,project_id,channel_id,issue_id,event,observed_at,created_at)
        VALUES ('page-newer',$1,$2,$3,'issue-new','alert',$4,$4)`, org, projects[0], channel, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	seen := append([]automationoutbox.DeliveryReceipt{}, first.Items...)
	cursor := first.NextCursor
	for cursor != "" {
		params := url.Values{"limit": {"17"}, "cursor": {cursor}}
		w, page := request(projects[0], viewer, params)
		if w.Code != 200 || len(page.Items) > 17 || len(page.Items) == 0 {
			t.Fatalf("older page: %d %+v", w.Code, page)
		}
		_, replay := request(projects[0], viewer, params)
		encoded, _ := json.Marshal(page)
		replayed, _ := json.Marshal(replay)
		if string(encoded) != string(replayed) {
			t.Fatal("replaying an unchanged cursor changed its page")
		}
		seen = append(seen, page.Items...)
		if len(seen) > 130 {
			t.Fatal("pagination repeated notices")
		}
		cursor = page.NextCursor
	}
	if len(seen) != 130 {
		t.Fatalf("keyset skipped notices: %d", len(seen))
	}
	for i, row := range seen {
		if row.NoticeID != fmt.Sprintf("page-main-%03d", 129-i) {
			t.Fatalf("unstable order at %d: %s", i, row.NoticeID)
		}
	}
	w, capped := request(projects[0], viewer, url.Values{"limit": {"100"}})
	if w.Code != 200 || len(capped.Items) != 100 || capped.Items[0].NoticeID != "page-newer" || capped.NextCursor == "" {
		t.Fatalf("maximum page: %d %+v", w.Code, capped)
	}
	w, filtered := request(projects[0], viewer, url.Values{"issue_id": {"issue-a"}, "limit": {"10"}})
	if w.Code != 200 || len(filtered.Items) != 10 || filtered.NextCursor == "" {
		t.Fatalf("issue page: %d %+v", w.Code, filtered)
	}
	for _, row := range filtered.Items {
		if row.IssueID != "issue-a" {
			t.Fatal("issue filter returned another issue")
		}
	}
	for _, invalid := range []url.Values{{"limit": {"0"}}, {"limit": {"-1"}}, {"limit": {"101"}}, {"limit": {"wat"}}, {"cursor": {"%%%"}}, {"cursor": {strings.Repeat("a", 513)}}, {"issue_id": {strings.Repeat("x", 129)}}, {"issue_id": {"issue-b"}, "cursor": {filtered.NextCursor}}, {"cursor": {filtered.NextCursor}}} {
		if result, _ := request(projects[0], viewer, invalid); result.Code != 400 {
			t.Fatalf("invalid pagination accepted: %d %s", result.Code, result.Body)
		}
	}
	for _, scope := range []struct{ project, token string }{{projects[1], viewer}, {foreignProjects[0], foreignViewer}} {
		if result, _ := request(scope.project, scope.token, url.Values{"cursor": {first.NextCursor}}); result.Code != 400 {
			t.Fatalf("foreign cursor accepted: %d %s", result.Code, result.Body)
		}
	}
	for _, scope := range []struct{ project, token string }{{projects[0], foreignViewer}, {foreignProjects[0], viewer}, {id.New(), viewer}} {
		if result, _ := request(scope.project, scope.token, nil); result.Code != 404 || strings.Contains(result.Body.String(), "page-main") {
			t.Fatalf("cross-scope listing accepted: %d %s", result.Code, result.Body)
		}
	}
	if result, _ := request(projects[0], "", nil); result.Code != 401 {
		t.Fatalf("unauthenticated listing: %d", result.Code)
	}
	w, empty := request(projects[1], viewer, nil)
	if w.Code != 200 || empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("empty list shape: %d %s", w.Code, w.Body)
	}
	if page, err := automationoutbox.New(pool).ListDeliveryReceipts(ctx, foreignOrg, projects[0], "", 25, ""); err != nil || len(page.Items) != 0 {
		t.Fatalf("store bypassed org scope: %+v %v", page, err)
	}
}

func deliveryFixtureAck(t *testing.T, n automationoutbox.Notice, deduped bool) *automationdelivery.Acknowledgement {
	t.Helper()
	body, err := automationdelivery.NoticeBody(n.ProjectID, n.IssueID, n.Event, n.EventCount, n.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"accepted":true,"deduped":false,"run_id":"run_exact123"}`
	if deduped {
		raw = strings.Replace(raw, `false`, `true`, 1)
	}
	if n.Event == "test" {
		raw = `{"accepted":true,"test":true,"dispatched":false}`
	}
	ack, err := automationdelivery.ParseReactor([]byte(raw), n.Event, 200, automationdelivery.Digest(body))
	if err != nil {
		t.Fatal(err)
	}
	return ack
}

func TestAutomationDeliveryAcknowledgementAtomicLeaseAndReplay(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	org, channel, projects := seedOrgProjectChannel(t, pool, 2)
	otherOrg, _, otherProjects := seedOrgProjectChannel(t, pool, 1)
	if _, err := pool.Exec(ctx, `UPDATE notification_channels SET type='automation_webhook',config='{"url":"https://receiver.invalid/private","signing_secret":"never-retain-this","receipt_mode":"reactor-v1"}' WHERE id=$1`, channel); err != nil {
		t.Fatal(err)
	}
	store := automationoutbox.New(pool)
	n := enqueueDeliveryFixture(t, pool, org, projects[0], channel, "new_issue")
	ack := deliveryFixtureAck(t, n, false)
	assertUnacknowledged := func() {
		t.Helper()
		r, err := store.GetDeliveryReceipt(ctx, org, projects[0], n.ID)
		if err != nil || r.State != "leased" || r.Acknowledgement != "unrecorded" || r.AcknowledgedAt != nil || r.HTTPStatus != nil || r.ReceiverRunID != "" || r.RequestSHA256 != "" {
			t.Fatalf("partial acknowledgement: %+v %v", r, err)
		}
	}
	bad := *ack
	bad.RequestSHA256 = strings.Repeat("b", 64)
	if err := store.Finish(ctx, n, true, false, &bad); !errors.Is(err, automationdelivery.ErrReceipt) {
		t.Fatalf("foreign request digest accepted: %v", err)
	}
	assertUnacknowledged()
	wrong := n
	wrong.OrgID = otherOrg
	if err := store.Finish(ctx, wrong, true, false, ack); !errors.Is(err, automationoutbox.ErrStaleLease) {
		t.Fatalf("cross-org completion accepted: %v", err)
	}
	assertUnacknowledged()
	changed := n
	changed.EventCount++
	if err := store.Finish(ctx, changed, true, false, deliveryFixtureAck(t, changed, false)); !errors.Is(err, automationoutbox.ErrStaleLease) {
		t.Fatalf("altered original notice accepted: %v", err)
	}
	assertUnacknowledged()
	// A PostgreSQL rejection at the acknowledgement write cannot commit the
	// delivered state or spend the lease separately from its receipt.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_delivery_ack() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.receiver_mode <> '' THEN RAISE EXCEPTION 'synthetic acknowledgement failure'; END IF; RETURN NEW; END $$;
CREATE TRIGGER reject_delivery_ack BEFORE UPDATE ON automation_alert_outbox FOR EACH ROW EXECUTE FUNCTION reject_delivery_ack()`); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, n, true, false, ack); err == nil {
		t.Fatal("injected receipt write failure accepted")
	}
	assertUnacknowledged()
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_delivery_ack ON automation_alert_outbox; DROP FUNCTION reject_delivery_ack()`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE automation_alert_outbox SET leased_until=now()-interval '1 second' WHERE id=$1`, n.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, n, true, false, ack); !errors.Is(err, automationoutbox.ErrStaleLease) {
		t.Fatalf("expired lease accepted receiver receipt: %v", err)
	}
	assertUnacknowledged()
	batch, err := store.ClaimDue(ctx, 1, time.Minute)
	if err != nil || len(batch) != 1 {
		t.Fatalf("restart claim: %+v %v", batch, err)
	}
	retry := batch[0]
	if retry.ID != n.ID || retry.LeaseToken == n.LeaseToken || !retry.ObservedAt.Equal(n.ObservedAt) {
		t.Fatal("retry changed original notice or reused lease")
	}
	if err := store.Finish(ctx, n, true, false, ack); !errors.Is(err, automationoutbox.ErrStaleLease) {
		t.Fatalf("old owner overwrote retry: %v", err)
	}
	if err := store.Finish(ctx, retry, true, false, deliveryFixtureAck(t, retry, true)); err != nil {
		t.Fatal(err)
	}
	r, err := store.GetDeliveryReceipt(ctx, org, projects[0], n.ID)
	if err != nil || r.State != "delivered" || r.Acknowledgement != "receiver_enqueued" || r.ReceiverRunID != "run_exact123" || r.ReceiverDeduped == nil || !*r.ReceiverDeduped || r.RequestSHA256 != ack.RequestSHA256 || r.AcknowledgedAt == nil || r.Attempts != 2 {
		t.Fatalf("durable replay receipt: %+v %v", r, err)
	}
	if err := store.Finish(ctx, retry, true, false, ack); !errors.Is(err, automationoutbox.ErrStaleLease) {
		t.Fatalf("completed lease accepted another receipt: %v", err)
	}
	for _, scope := range [][2]string{{otherOrg, projects[0]}, {org, projects[1]}, {org, otherProjects[0]}} {
		if _, err := store.GetDeliveryReceipt(ctx, scope[0], scope[1], n.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("cross-scope read accepted: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE automation_alert_outbox SET receiver_http_status=NULL WHERE id=$1`, n.ID); err == nil {
		t.Fatal("database allowed an incomplete acknowledgement")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM notification_channels WHERE id=$1`, channel); err != nil {
		t.Fatal(err)
	}
	r, err = store.GetDeliveryReceipt(ctx, org, projects[0], n.ID)
	if err != nil || r.ChannelID != nil || r.ReceiverRunID != "run_exact123" {
		t.Fatalf("channel deletion erased acknowledgement: %+v %v", r, err)
	}
}

func TestAutomationDeliveryReceiptRegisteredViewerScopeAndKinds(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	q := generated.New(pool)
	org, channel, projects := seedOrgProjectChannel(t, pool, 2)
	foreignOrg, _, foreignProjects := seedOrgProjectChannel(t, pool, 1)
	if _, err := pool.Exec(ctx, `UPDATE notification_channels SET type='automation_webhook' WHERE id=$1`, channel); err != nil {
		t.Fatal(err)
	}
	store := automationoutbox.New(pool)
	n := enqueueDeliveryFixture(t, pool, org, projects[0], channel, "new_issue")
	if err := store.Finish(ctx, n, true, false, deliveryFixtureAck(t, n, false)); err != nil {
		t.Fatal(err)
	}
	mint := func(org string) string {
		t.Helper()
		token := "synthetic-viewer-" + id.New()
		if _, err := q.CreateAPIKey(ctx, generated.CreateAPIKeyParams{ID: id.New(), OrgID: org, Name: "delivery viewer", KeyHash: auth.HashAPIKey(token), KeyPrefix: "synthetic", Role: "viewer"}); err != nil {
			t.Fatal(err)
		}
		return token
	}
	viewer, foreignViewer := mint(org), mint(foreignOrg)
	s := serverForDB(pool)
	s.sessions = auth.NewSessionManager(time.Hour, time.Hour, false)
	handler := s.Routes(fstest.MapFS{"index.html": {Data: []byte("test")}}, func(next http.Handler) http.Handler { return next })
	request := func(project, notice, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/projects/"+project+"/automation-deliveries/"+notice, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	w := request(projects[0], n.ID, viewer)
	var response automationoutbox.DeliveryReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 || response.NoticeID != n.ID || response.Acknowledgement != "receiver_enqueued" || response.ReceiverRunID != "run_exact123" {
		t.Fatalf("viewer read: %d %s %v", w.Code, w.Body, err)
	}
	for _, forbidden := range []string{"org_id", "project_id", "lease_token", "signing_secret", "provider", "workflow_succeeded", "fix", "url", "private"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatalf("delivery read retained forbidden data %q: %s", forbidden, w.Body)
		}
	}
	for _, scope := range []struct{ project, notice, token string }{{projects[0], n.ID, foreignViewer}, {projects[1], n.ID, viewer}, {foreignProjects[0], n.ID, viewer}, {id.New(), n.ID, viewer}, {projects[0], id.New(), viewer}} {
		result := request(scope.project, scope.notice, scope.token)
		if result.Code != 404 || strings.Contains(result.Body.String(), n.ID) || strings.Contains(result.Body.String(), "run_exact123") {
			t.Fatalf("scope refusal: %d %s", result.Code, result.Body)
		}
	}
	if result := request(projects[0], n.ID, ""); result.Code != 401 {
		t.Fatalf("unauthenticated read: %d %s", result.Code, result.Body)
	}
	for _, event := range []string{"test", "alert"} {
		next := enqueueDeliveryFixture(t, pool, org, projects[0], channel, event)
		ack := deliveryFixtureAck(t, next, false)
		want := "test_noop"
		if event == "alert" {
			want = "http_delivery"
			ack = &automationdelivery.Acknowledgement{Mode: automationdelivery.HTTP, HTTPStatus: 204, RequestSHA256: ack.RequestSHA256}
		}
		if err := store.Finish(ctx, next, true, false, ack); err != nil {
			t.Fatal(err)
		}
		result := request(projects[0], next.ID, viewer)
		var row automationoutbox.DeliveryReceipt
		if err := json.Unmarshal(result.Body.Bytes(), &row); err != nil || result.Code != 200 || row.Acknowledgement != want || row.ReceiverRunID != "" || row.ReceiverDeduped != nil {
			t.Fatalf("%s asserted enqueue: %d %+v %v", event, result.Code, row, err)
		}
	}
}
