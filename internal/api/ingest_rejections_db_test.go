package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

func TestSentryRejectionReasonsAreDurablePrivateAndScoped(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	foreign := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	s.sessions = auth.NewSessionManager(time.Hour, time.Hour, false)
	router := s.Routes(fstest.MapFS{"index.html": {Data: []byte("test")}}, func(next http.Handler) http.Handler { return next })
	const canary = "PRIVATE_CREDENTIAL_CANARY_274893"
	body := envelopeBody(
		[2]string{"event", `{"event_id":"reason-accepted","message":"synthetic"}`},
		[2]string{"event", `{`},
		[2]string{"transaction", `{`},
		[2]string{"client_report", `{`},
		[2]string{"session", `{"secret":"` + canary + `"}`},
		[2]string{"sessions", `{}`},
		[2]string{"attachment", `{"password":"` + canary + `"}`},
		[2]string{canary, `{}`},
	)
	res := sendEnvelope(s, project.PublicKey, body)
	if res.Code != http.StatusOK {
		t.Fatalf("mixed ingest = %d %s", res.Code, res.Body.String())
	}
	s.bgWG.Wait()
	rows, err := s.q.ListIngestReceiptsByProject(ctx, generated.ListIngestReceiptsByProjectParams{OrgID: project.OrgID, ProjectID: project.ID, Limit: 100})
	if err != nil || len(rows) != 1 {
		t.Fatalf("receipts = %d err=%v", len(rows), err)
	}
	want := map[string]int64{"invalid_event": 1, "invalid_transaction": 1, "invalid_client_report": 1,
		"unsupported_session": 2, "unsupported_attachment": 1, "unsupported_other": 1}
	row := rows[0]
	if row.Outcome != "partial" || row.AcceptedCount != 1 || row.RejectedCount != 7 || row.CompletedAt.Valid == false || !reflect.DeepEqual(safeIngestRejections(row.RejectionCounts), want) {
		t.Fatalf("mixed receipt accounting = %+v", row)
	}
	if strings.Contains(string(row.RejectionCounts), canary) {
		t.Fatal("receipt persisted an arbitrary SDK type or payload")
	}
	viewer := "synthetic-intake-viewer-" + id.New()
	if _, err := s.q.CreateAPIKey(ctx, generated.CreateAPIKeyParams{ID: id.New(), OrgID: project.OrgID,
		Name: "intake viewer", KeyHash: auth.HashAPIKey(viewer), KeyPrefix: "synthetic", Role: "viewer"}); err != nil {
		t.Fatal(err)
	}
	get := func(projectID, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/ingest-receipts", nil)
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	read := get(project.ID, viewer)
	var apiRows []ingestReceiptResponse
	if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &apiRows) != nil || len(apiRows) != 1 || !reflect.DeepEqual(apiRows[0].RejectionCounts, want) {
		t.Fatalf("viewer receipt = %d %s", read.Code, read.Body.String())
	}
	for _, forbidden := range []string{canary, project.OrgID, project.PublicKey, "synthetic-intake-viewer"} {
		if strings.Contains(read.Body.String(), forbidden) {
			t.Fatal("viewer response exposed private values")
		}
	}
	if read := get(project.ID, ""); read.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous read = %d", read.Code)
	}
	// Both an inaccessible and an absent project return an empty scoped list.
	for _, pid := range []string{foreign.ID, id.New()} {
		read := get(pid, viewer)
		if read.Code != http.StatusOK || strings.TrimSpace(read.Body.String()) != "[]" {
			t.Fatalf("foreign read = %d %s", read.Code, read.Body.String())
		}
	}
	// Defaults preserve historical truth; do not infer a reason from old counts.
	if _, err := pool.Exec(ctx, `INSERT INTO ingest_receipts (id,org_id,project_id,pillar,protocol,outcome,http_status,rejected_count)
		VALUES ($1,$2,$3,'envelope','sentry','rejected',400,5)`, id.New(), project.OrgID, project.ID); err != nil {
		t.Fatal(err)
	}
	var historical json.RawMessage
	if err := pool.QueryRow(ctx, `SELECT rejection_counts FROM ingest_receipts WHERE project_id=$1 AND rejected_count=5`, project.ID).Scan(&historical); err != nil || string(historical) != "{}" {
		t.Fatalf("historical reasons = %s err=%v", historical, err)
	}
}

func TestSentryRejectionReasonsFollowAtomicReceiptAndSchemaBounds(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	body := envelopeBody([2]string{"event", `{"event_id":"reason-atomic","message":"synthetic"}`}, [2]string{"session", `{}`})
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_reason_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic receipt failure'; END $$;
		CREATE TRIGGER fail_reason_receipt BEFORE UPDATE ON ingest_receipts FOR EACH ROW EXECUTE FUNCTION fail_reason_receipt()`); err != nil {
		t.Fatal(err)
	}
	res := sendEnvelope(s, project.PublicKey, body)
	if res.Code != http.StatusServiceUnavailable || countEvents(t, pool, project.ID) != 0 {
		t.Fatalf("receipt rollback = %d %s", res.Code, res.Body.String())
	}
	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ingest_receipts WHERE project_id=$1 AND outcome='in_progress' AND rejection_counts='{}'::jsonb`, project.ID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending = %d err=%v", pending, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_reason_receipt ON ingest_receipts; DROP FUNCTION fail_reason_receipt()`); err != nil {
		t.Fatal(err)
	}
	res = sendEnvelope(s, project.PublicKey, body)
	if res.Code != http.StatusOK || countEvents(t, pool, project.ID) != 1 {
		t.Fatalf("retry = %d %s", res.Code, res.Body.String())
	}
	s.bgWG.Wait()
	var receiptID string
	if err := pool.QueryRow(ctx, `SELECT id FROM ingest_receipts WHERE project_id=$1 AND outcome='partial' AND rejection_counts='{"unsupported_session":1}'::jsonb`, project.ID).Scan(&receiptID); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`null`, `[]`, `{"arbitrary_secret":1}`, `{"unsupported_session":"private"}`, `{"unsupported_session":{}}`, `{"unsupported_session":0}`, `{"unsupported_session":-1}`, `{"unsupported_session":1.5}`, `{"unsupported_session":1001}`} {
		if _, err := pool.Exec(ctx, `UPDATE ingest_receipts SET rejection_counts=$1::jsonb WHERE id=$2`, bad, receiptID); err == nil {
			t.Fatalf("database accepted unbounded reasons %s", bad)
		}
	}
	// An empty result remains valid for other receiver protocols. Callers that
	// do not provide this new optional query argument keep their old contract.
	if _, err := s.q.CompleteIngestReceipt(ctx, generated.CompleteIngestReceiptParams{ID: receiptID, OrgID: project.OrgID,
		ProjectID: project.ID, Outcome: "accepted", HttpStatus: http.StatusOK}); err != nil {
		t.Fatal(err)
	}
}

func TestSentryMalformedEnvelopeReasonDoesNotRetainInput(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	res := sendEnvelope(s, project.PublicKey, "PRIVATE_MALFORMED_INPUT")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("malformed envelope = %d", res.Code)
	}
	var reasons json.RawMessage
	if err := pool.QueryRow(context.Background(), `SELECT rejection_counts FROM ingest_receipts WHERE project_id=$1`, project.ID).Scan(&reasons); err != nil || !reflect.DeepEqual(safeIngestRejections(reasons), map[string]int64{"invalid_envelope": 1}) {
		t.Fatalf("malformed reasons = %s err=%v", reasons, err)
	}
}
