package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sentry "github.com/getsentry/sentry-go"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
)

// RFC3339 is the wire encoding used by sentry-go 0.47.0 and sentry-dart
// 9.21.0. The browser numeric form is covered by the parser tests.
const sdkReportFixture = `{"timestamp":"2025-02-07T14:16:00.123Z","discarded_events":[{"reason":"sample_rate","category":"transaction","quantity":12},{"reason":"network_error","category":"error","quantity":3}]}`

type sdkReportRoundTripper func(*http.Request) (*http.Response, error)

func (f sdkReportRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Run the actual first-party SDK without a socket or external service: its
// generated event+client_report envelope reaches the real DB-backed handler.
func TestSentryClientReportOfficialGoSDKEnvelopeAccepted(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	var mu sync.Mutex
	var responses []map[string]any
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn: "https://" + project.PublicKey + "@sdk.invalid/1",
		BeforeSend: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			if event.Message == "synthetic-drop" {
				return nil
			}
			return event
		},
		HTTPClient: &http.Client{Transport: sdkReportRoundTripper(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			rec := sendEnvelope(s, project.PublicKey, string(body))
			var response map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				return nil, err
			}
			mu.Lock()
			responses = append(responses, response)
			mu.Unlock()
			return &http.Response{StatusCode: rec.Code, Header: rec.Header(), Body: io.NopCloser(strings.NewReader(rec.Body.String())), Request: req}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	hub := sentry.NewHub(client, sentry.NewScope())
	hub.CaptureMessage("synthetic-drop")
	hub.CaptureMessage("synthetic-accepted")
	if !client.Flush(3 * time.Second) {
		t.Fatal("SDK flush timed out")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(responses) != 1 || responses[0]["accepted"] != float64(2) || responses[0]["invalid"] != float64(0) {
		t.Fatalf("sentry-go %s event+report result = %#v", sentry.SDKVersion, responses)
	}
	var reason, category string
	var quantity int64
	if err := pool.QueryRow(context.Background(), `SELECT reason,category,quantity FROM sentry_client_report_discards
		WHERE org_id=$1 AND project_id=$2`, project.OrgID, project.ID).Scan(&reason, &category, &quantity); err != nil || reason != "before_send" || category != "error" || quantity != 1 {
		t.Fatalf("SDK-generated counter = %s/%s/%d: %v", reason, category, quantity, err)
	}
}

func sdkDiscardsRequest(s *Server, orgID, projectID, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/sdk-discards"+query, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", projectID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	if orgID != "" {
		ctx = context.WithValue(ctx, ctxOrgID, orgID)
	}
	rec := httptest.NewRecorder()
	s.handleListSDKDiscards(rec, req.WithContext(ctx))
	return rec
}

func TestSentryClientReportStandaloneReplayAndMixedEnvelope(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	body := envelopeBody([2]string{"client_report", sdkReportFixture})
	for i := 0; i < 2; i++ {
		rec := sendEnvelope(s, project.PublicKey, body)
		result := envelopeResult(t, rec.Body.String())
		if rec.Code != 200 || result["accepted"] != float64(1) || result["invalid"] != float64(0) || result["deduplicated"] != float64(i) || result["persisted"] != float64(1-i) {
			t.Fatalf("standalone send %d = %d %#v", i, rec.Code, result)
		}
	}
	mixed := envelopeBody([2]string{"event", `{"event_id":"sdk-event","message":"SDK event plus report"}`}, [2]string{"client_report", sdkReportFixture})
	rec := sendEnvelope(s, project.PublicKey, mixed)
	result := envelopeResult(t, rec.Body.String())
	if rec.Code != 200 || result["accepted"] != float64(2) || result["persisted"] != float64(1) || result["deduplicated"] != float64(1) {
		t.Fatalf("mixed SDK envelope = %d %#v", rec.Code, result)
	}
	for table, want := range map[string]int{"sentry_client_report_discards": 2, "sentry_item_acceptances": 2, "events": 1} {
		if got := countEnvelopeRows(t, pool, table, project.ID); got != want {
			t.Fatalf("%s rows = %d, want %d", table, got, want)
		}
	}
	var received, accepted, persisted, rejected int64
	var terminal bool
	if err := pool.QueryRow(context.Background(), `SELECT received_count,accepted_count,persisted_count,rejected_count,completed_at IS NOT NULL
		FROM ingest_receipts WHERE org_id=$1 AND project_id=$2 ORDER BY received_at DESC LIMIT 1`, project.OrgID, project.ID).Scan(&received, &accepted, &persisted, &rejected, &terminal); err != nil {
		t.Fatal(err)
	}
	if received != 2 || accepted != 2 || persisted != 1 || rejected != 0 || !terminal {
		t.Fatalf("mixed terminal receipt = %d/%d/%d/%d %t", received, accepted, persisted, rejected, terminal)
	}
	// The SDK clock is old; the read window deliberately uses receiver time.
	read := sdkDiscardsRequest(s, project.OrgID, project.ID, "?hours=1")
	var response struct {
		Source    string                `json:"source_kind"`
		Basis     string                `json:"window_basis"`
		Breakdown []sdkDiscardBreakdown `json:"breakdown"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &response); err != nil || read.Code != 200 || response.Source != "client_reported" || response.Basis != "received_at" || len(response.Breakdown) != 2 {
		t.Fatalf("SDK read = %d %s: %v", read.Code, read.Body.String(), err)
	}
	for _, row := range response.Breakdown {
		if row.Reports != 1 || row.FirstReceived == nil || row.LastReceived == nil {
			t.Fatalf("replay multiplied report counts or lost freshness: %#v", row)
		}
		if row.Reason == "sample_rate" && (row.Kind != "sampling" || row.Quantity != "12") || row.Reason == "network_error" && (row.Kind != "delivery" || row.Quantity != "3") {
			t.Fatalf("wrong discard classification: %#v", row)
		}
	}
	// The existing accepted-item janitor can expire a report without leaving
	// counters that outlive its replay/provenance ledger.
	if _, err := pool.Exec(context.Background(), `DELETE FROM sentry_item_acceptances
		WHERE org_id=$1 AND project_id=$2 AND item_type='client_report'`, project.OrgID, project.ID); err != nil {
		t.Fatal(err)
	}
	if got := countEnvelopeRows(t, pool, "sentry_client_report_discards", project.ID); got != 0 {
		t.Fatalf("expired acceptance left %d orphan counters", got)
	}
}

func TestSentryClientReportFutureClockUsesReceiverWindowAndReplaysOnce(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	future := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	payload := fmt.Sprintf(`{"timestamp":%q,"discarded_events":[{"reason":"network_error","category":"error","quantity":3}]}`, future.Format(time.RFC3339Nano))
	body := envelopeBody([2]string{"client_report", payload})
	before := time.Now().UTC().Add(-time.Second)
	for i := 0; i < 2; i++ {
		rec := sendEnvelope(s, project.PublicKey, body)
		result := envelopeResult(t, rec.Body.String())
		if rec.Code != http.StatusOK || result["accepted"] != float64(1) || result["invalid"] != float64(0) || result["persisted"] != float64(1-i) || result["deduplicated"] != float64(i) {
			t.Fatalf("future-clock send %d = %d %#v", i, rec.Code, result)
		}
	}
	var reportedAt, receivedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT reported_at, received_at FROM sentry_client_report_discards
		WHERE org_id=$1 AND project_id=$2`, project.OrgID, project.ID).Scan(&reportedAt, &receivedAt); err != nil || !reportedAt.Equal(future) || receivedAt.Before(before) || receivedAt.After(time.Now().UTC()) {
		t.Fatalf("separate client/receiver clocks = %s/%s: %v", reportedAt, receivedAt, err)
	}
	read := sdkDiscardsRequest(s, project.OrgID, project.ID, "?hours=1")
	var response struct {
		Basis     string                `json:"window_basis"`
		Breakdown []sdkDiscardBreakdown `json:"breakdown"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &response); err != nil || read.Code != http.StatusOK || response.Basis != "received_at" || len(response.Breakdown) != 1 {
		t.Fatalf("future-clock read = %d %s: %v", read.Code, read.Body.String(), err)
	}
	row := response.Breakdown[0]
	if row.Quantity != "3" || row.Reports != 1 || row.FirstReceived == nil || row.LastReceived == nil || !row.FirstReceived.Equal(receivedAt) || !row.LastReceived.Equal(receivedAt) {
		t.Fatalf("future clock or replay changed counters/freshness: %#v", row)
	}
	if got := countEnvelopeRows(t, pool, "sentry_item_acceptances", project.ID); got != 1 {
		t.Fatalf("future-clock replay identities = %d", got)
	}
	// A future client timestamp must not keep an old receipt in a fresh window.
	// Without this stale-receiver check, a query using reported_at >= since
	// would also pass the fresh read above and conceal the clock-basis defect.
	if _, err := pool.Exec(ctx, `UPDATE sentry_client_report_discards SET received_at=now()-interval '2 hours'
		WHERE org_id=$1 AND project_id=$2`, project.OrgID, project.ID); err != nil {
		t.Fatal(err)
	}
	read = sdkDiscardsRequest(s, project.OrgID, project.ID, "?hours=1")
	if err := json.Unmarshal(read.Body.Bytes(), &response); err != nil || read.Code != http.StatusOK || len(response.Breakdown) != 0 {
		t.Fatalf("future client clock kept an old receipt fresh: %d %s: %v", read.Code, read.Body.String(), err)
	}
}

func TestSentryClientReportAndEventRollbackOnCounterStoreFailure(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_client_report() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.reason='network_error' THEN RAISE EXCEPTION 'synthetic counter failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_client_report BEFORE INSERT ON sentry_client_report_discards FOR EACH ROW EXECUTE FUNCTION fail_client_report()`); err != nil {
		t.Fatal(err)
	}
	body := envelopeBody([2]string{"event", `{"event_id":"report-atomic","message":"report atomic"}`}, [2]string{"client_report", sdkReportFixture})
	rec := sendEnvelope(s, project.PublicKey, body)
	result := envelopeResult(t, rec.Body.String())
	if rec.Code != 503 || result["accepted"] != float64(0) || result["failed"] != float64(2) || result["retryable"] != true {
		t.Fatalf("store failure = %d %#v", rec.Code, result)
	}
	for _, table := range []string{"events", "issues", "sentry_item_acceptances", "sentry_client_report_discards"} {
		if got := countEnvelopeRows(t, pool, table, project.ID); got != 0 {
			t.Fatalf("%s survived atomic rollback (%d rows)", table, got)
		}
	}
	var failed int64
	var retryable bool
	if err := pool.QueryRow(ctx, `SELECT rejected_count,retryable FROM ingest_receipts WHERE project_id=$1 AND org_id=$2`, project.ID, project.OrgID).Scan(&failed, &retryable); err != nil || failed != 2 || !retryable {
		t.Fatalf("failed receipt = %d/%t: %v", failed, retryable, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_client_report ON sentry_client_report_discards; DROP FUNCTION fail_client_report()`); err != nil {
		t.Fatal(err)
	}
	// A caller-controlled retry is safe. This does not assert that every SDK
	// retries HTTP503; the Sentry protocol allows SDKs to discard on HTTP5xx.
	if rec = sendEnvelope(s, project.PublicKey, body); rec.Code != 200 {
		t.Fatalf("explicit retry = %d %s", rec.Code, rec.Body.String())
	}
	if got := countEnvelopeRows(t, pool, "sentry_client_report_discards", project.ID); got != 2 {
		t.Fatalf("explicit retry counters = %d", got)
	}
}

func TestSentryClientReportTerminalReceiptFailureRollsBackCounters(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_client_report_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic terminal receipt failure'; END $$;
		CREATE TRIGGER fail_client_report_receipt BEFORE UPDATE ON ingest_receipts FOR EACH ROW EXECUTE FUNCTION fail_client_report_receipt()`); err != nil {
		t.Fatal(err)
	}
	rec := sendEnvelope(s, project.PublicKey, envelopeBody([2]string{"client_report", sdkReportFixture}))
	result := envelopeResult(t, rec.Body.String())
	if rec.Code != 503 || result["accepted"] != float64(0) || result["failed"] != float64(1) {
		t.Fatalf("receipt failure = %d %#v", rec.Code, result)
	}
	if got := countEnvelopeRows(t, pool, "sentry_client_report_discards", project.ID); got != 0 {
		t.Fatalf("counters survived terminal receipt failure: %d", got)
	}
	if got := countEnvelopeRows(t, pool, "sentry_item_acceptances", project.ID); got != 0 {
		t.Fatalf("claim survived terminal receipt failure: %d", got)
	}
	if got := sentryReceiptCount(t, pool, project, "envelope", "in_progress"); got != 1 {
		t.Fatalf("pending receipt count = %d", got)
	}
}

func TestSentryClientReportMalformedPartialAndTenantIsolation(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	first, second := seedProjectForIngest(t, pool), seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	badReport := `{"discarded_events":[{"reason":"network_error","category":"error","quantity":-1}]}`
	for _, p := range []*generated.Project{first, second} {
		rec := sendEnvelope(s, p.PublicKey, envelopeBody([2]string{"client_report", sdkReportFixture}, [2]string{"client_report", badReport}))
		result := envelopeResult(t, rec.Body.String())
		if rec.Code != 200 || result["accepted"] != float64(1) || result["invalid"] != float64(1) || result["partial"] != true {
			t.Fatalf("partial report = %d %#v", rec.Code, result)
		}
		if got := countEnvelopeRows(t, pool, "sentry_client_report_discards", p.ID); got != 2 {
			t.Fatalf("project-scoped reports = %d", got)
		}
	}
	rows, err := s.q.ListSentryClientReportDiscards(ctx, generated.ListSentryClientReportDiscardsParams{OrgID: second.OrgID, ProjectID: first.ID, ReceivedAt: pgtype.Timestamptz{Time: first.CreatedAt.Time, Valid: true}})
	if err != nil || len(rows) != 0 {
		t.Fatalf("cross-tenant query leaked %d rows: %v", len(rows), err)
	}
	if read := sdkDiscardsRequest(s, second.OrgID, first.ID, ""); read.Code != 404 {
		t.Fatalf("cross-tenant read = %d %s", read.Code, read.Body.String())
	}
	if read := sdkDiscardsRequest(s, "", first.ID, ""); read.Code != 401 {
		t.Fatalf("unauthenticated read = %d", read.Code)
	}
	for _, query := range []string{"?hours=-1", "?hours=169", "?hours=garbage"} {
		if read := sdkDiscardsRequest(s, first.OrgID, first.ID, query); read.Code != 400 {
			t.Fatalf("invalid window %s = %d", query, read.Code)
		}
	}
	// DB ownership is enforced even if a future caller misuses the writer.
	_, err = pool.Exec(ctx, `INSERT INTO sentry_client_report_discards(org_id,project_id,payload_sha256,reported_at,reason,category,quantity)
		VALUES($1,$2,$3,now(),'send_error','error',1)`, second.OrgID, first.ID, sentryPayloadHash([]byte(sdkReportFixture)))
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23503" || pgerr.ConstraintName != "sentry_client_reports_project_scope_fk" {
		t.Fatalf("database did not enforce tenant ownership FK: %v", err)
	}
	invalid := sendEnvelope(s, first.PublicKey, envelopeBody([2]string{"client_report", badReport}))
	if invalid.Code != 400 || !strings.Contains(invalid.Body.String(), `"accepted":0`) {
		t.Fatalf("invalid-only report = %d %s", invalid.Code, invalid.Body.String())
	}
}

func TestSentryClientReportConcurrentReplayPersistsCountersOnce(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	body := envelopeBody([2]string{"client_report", sdkReportFixture})
	var wg sync.WaitGroup
	statuses := make([]int, 6)
	for i := range statuses {
		wg.Add(1)
		go func(i int) { defer wg.Done(); statuses[i] = sendEnvelope(s, project.PublicKey, body).Code }(i)
	}
	wg.Wait()
	for _, status := range statuses {
		if status != 200 {
			t.Fatalf("concurrent report status = %d", status)
		}
	}
	if got := countEnvelopeRows(t, pool, "sentry_client_report_discards", project.ID); got != 2 {
		t.Fatalf("concurrent report counters = %d", got)
	}
	if got := countEnvelopeRows(t, pool, "sentry_item_acceptances", project.ID); got != 1 {
		t.Fatalf("concurrent report identities = %d", got)
	}
}

func TestSentryClientReportSharesSpanBudgetAndCanRetryAfterDrop(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	// One root + children consume the existing request row cap. The following
	// report must be visibly dropped, without reserving its replay identity.
	var transaction strings.Builder
	transaction.WriteString(`{"event_id":"full-budget","transaction":"budget fixture","start_timestamp":1738937760,"timestamp":1738937761,"contexts":{"trace":{"trace_id":"budget-trace","span_id":"root","op":"http.server"}},"spans":[`)
	for i := 0; i < maxIngestRecords-1; i++ {
		if i > 0 {
			transaction.WriteByte(',')
		}
		fmt.Fprintf(&transaction, `{"span_id":"child-%d","start_timestamp":1738937760,"timestamp":1738937761}`, i)
	}
	transaction.WriteString(`]}`)
	rec := sendEnvelope(s, project.PublicKey, envelopeBody([2]string{"transaction", transaction.String()}, [2]string{"client_report", sdkReportFixture}))
	result := envelopeResult(t, rec.Body.String())
	if rec.Code != 200 || result["accepted"] != float64(1) || result["dropped"] != float64(1) || result["partial"] != true {
		t.Fatalf("report at row cap = %d %#v", rec.Code, result)
	}
	if got := countEnvelopeRows(t, pool, "spans", project.ID); got != maxIngestRecords {
		t.Fatalf("persisted span rows = %d, want cap %d", got, maxIngestRecords)
	}
	if got := countEnvelopeRows(t, pool, "sentry_client_report_discards", project.ID); got != 0 {
		t.Fatalf("over-budget report wrote %d counters", got)
	}
	if got := countEnvelopeRows(t, pool, "sentry_item_acceptances", project.ID); got != 1 {
		t.Fatalf("dropped report kept replay claim: %d identities", got)
	}
	var accepted, dropped int64
	if err := pool.QueryRow(context.Background(), `SELECT accepted_count,dropped_count FROM ingest_receipts
		WHERE org_id=$1 AND project_id=$2`, project.OrgID, project.ID).Scan(&accepted, &dropped); err != nil || accepted != 1 || dropped != 1 {
		t.Fatalf("budget receipt = %d/%d: %v", accepted, dropped, err)
	}
	retry := sendEnvelope(s, project.PublicKey, envelopeBody([2]string{"client_report", sdkReportFixture}))
	retryResult := envelopeResult(t, retry.Body.String())
	if retry.Code != 200 || retryResult["persisted"] != float64(1) || retryResult["deduplicated"] != float64(0) {
		t.Fatalf("previously dropped report retry = %d %#v", retry.Code, retryResult)
	}
}
