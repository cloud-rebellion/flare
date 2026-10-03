package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// These tests drive the REAL handleStore / handleEnvelope against a real
// postgres:16, using the same throwaway-database harness as
// update_channel_tx_test.go (newTestDB / serverForDB, gated on
// FLARE_TEST_DATABASE_URL). Run them with, e.g.:
//
//	FLARE_TEST_DATABASE_URL='postgres://test:test@127.0.0.1:55439/flare?sslmode=disable' \
//	  go test ./internal/api/ -run 'HandleStore|HandleEnvelope' -count=1 -v
//
// They pin two findings on the event ingest path:
//   - B1: the events path was the only telemetry path that skipped NUL / invalid
//     UTF-8 sanitization, so an event carrying a backslash-u-0000 escape failed
//     the INSERT and the SDK dropped it (silent per-event data loss).
//   - B2: handleStore mapped ANY ingestOne error to 400, so a server-side store
//     failure was reported as a malformed body, which Sentry SDKs drop instead of
//     retrying. A store failure must be 5xx.

// nulEscape is the six literal characters of a JSON NUL escape. Kept as a raw
// string constant so this test file never embeds an actual NUL byte.
const nulEscape = "\\u0000"

// seedProjectForIngest inserts one org and one project with a known public key,
// returning the project so a test can authenticate an ingest request.
func seedProjectForIngest(t *testing.T, pool *pgxpool.Pool) *generated.Project {
	t.Helper()
	ctx := context.Background()
	org := id.New()
	if _, err := pool.Exec(ctx, "INSERT INTO orgs (id, name, slug) VALUES ($1,$2,$3)", org, "Acme", "acme-"+org); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	p, err := generated.New(pool).CreateProject(ctx, generated.CreateProjectParams{
		ID: id.New(), OrgID: org, Name: "proj", Slug: "proj-" + id.New(),
		Platform: "other", PublicKey: id.New(), DsnID: id.New(),
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return p
}

// storeEvent drives the real handleStore with the given ingest key and body.
func storeEvent(s *Server, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/store/", strings.NewReader(body))
	req.Header.Set("X-Flare-Key", key)
	rec := httptest.NewRecorder()
	s.handleStore(rec, req)
	return rec
}

// sendEnvelope drives the real handleEnvelope with the given ingest key and body.
func sendEnvelope(s *Server, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/envelope/", strings.NewReader(body))
	req.Header.Set("X-Flare-Key", key)
	rec := httptest.NewRecorder()
	s.handleEnvelope(rec, req)
	return rec
}

func countEvents(t *testing.T, pool *pgxpool.Pool, projectID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM events WHERE project_id=$1", projectID).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// TestHandleStoreSanitizesNulBytes is the B1 proof. An event whose message,
// exception type, and exception value all carry a backslash-u-0000 escape (which
// decodes to a NUL byte in the Go string, and appears literally in the JSONB
// payload) must now be STORED, not rejected. On the pre-fix code the raw fields
// reached Postgres and the INSERT failed, so handleStore answered non-200 and
// the SDK dropped the event.
func TestHandleStoreSanitizesNulBytes(t *testing.T) {
	pool := newTestDB(t)
	proj := seedProjectForIngest(t, pool)
	s := serverForDB(pool)

	// backslash-u-0000 in message, exception type, and exception value. The raw
	// body text also carries the escapes, so the JSONB payload column is
	// exercised too (Postgres rejects the NUL escape as "cannot be converted to
	// text" there).
	body := `{"event_id":"abc123","message":"boom` + nulEscape + `bar",` +
		`"exception":{"values":[{"type":"Err` + nulEscape + `Type","value":"val` + nulEscape + `ue"}]}}`

	rec := storeEvent(s, proj.PublicKey, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (event stored after sanitize), got %d (%s)", rec.Code, rec.Body.String())
	}
	s.bgWG.Wait()

	if got := countEvents(t, pool, proj.ID); got != 1 {
		t.Fatalf("want 1 stored event, got %d: the sanitized INSERT did not land", got)
	}

	// The stored text columns must be NUL-free and equal the sanitized value.
	var msg, excType, excValue string
	if err := pool.QueryRow(context.Background(),
		"SELECT message, exception_type, exception_value FROM events WHERE project_id=$1",
		proj.ID).Scan(&msg, &excType, &excValue); err != nil {
		t.Fatalf("read stored event: %v", err)
	}
	for name, v := range map[string]string{"message": msg, "exception_type": excType, "exception_value": excValue} {
		if strings.IndexByte(v, 0) >= 0 {
			t.Fatalf("%s still contains a NUL byte after storage: %q", name, v)
		}
	}
	if msg != "boombar" || excType != "ErrType" || excValue != "value" {
		t.Fatalf("stored fields not sanitized as expected: message=%q type=%q value=%q", msg, excType, excValue)
	}

	// The JSONB payload must be present and valid (readable as text without a
	// conversion error), and carry no NUL escape or raw NUL.
	var payload string
	if err := pool.QueryRow(context.Background(),
		"SELECT payload::text FROM events WHERE project_id=$1", proj.ID).Scan(&payload); err != nil {
		t.Fatalf("read stored payload: %v", err)
	}
	if strings.Contains(payload, nulEscape) || strings.IndexByte(payload, 0) >= 0 {
		t.Fatalf("stored JSONB payload still carries a NUL: %q", payload)
	}
}

// TestHandleEnvelopeFiftyEventBatch proves the sanitize change did not break the
// legitimate batch path: a real 50-event envelope of clean events still stores
// all 50 and answers 200.
func TestHandleEnvelopeFiftyEventBatch(t *testing.T) {
	pool := newTestDB(t)
	proj := seedProjectForIngest(t, pool)
	s := serverForDB(pool)

	var b strings.Builder
	b.WriteString("{}\n") // envelope header
	const n = 50
	for i := 0; i < n; i++ {
		b.WriteString(`{"type":"event"}` + "\n")
		fmt.Fprintf(&b, `{"event_id":%q,"message":"clean event %d"}`+"\n", id.New(), i)
	}

	rec := sendEnvelope(s, proj.PublicKey, b.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for a 50-event batch, got %d (%s)", rec.Code, rec.Body.String())
	}
	s.bgWG.Wait()

	if got := countEvents(t, pool, proj.ID); got != n {
		t.Fatalf("want %d stored events, got %d", n, got)
	}
}

// TestHandleStoreErrorClass is the B2 proof. A client parse failure must be 4xx
// (the SDK is right to drop a malformed body); a server-side store/DB failure
// must be 5xx (so the SDK retries instead of dropping a good event). On the
// pre-fix code both returned 400, so the "store failure is 5xx" case asserts RED.
func TestHandleStoreErrorClass(t *testing.T) {
	const validEvent = `{"event_id":"e1","message":"hello"}`
	cases := []struct {
		name     string
		body     string
		breakDB  bool
		wantCode int
	}{
		{"parse error is 4xx", "this is not json", false, http.StatusBadRequest},
		{"store failure is 5xx", validEvent, true, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestDB(t)
			proj := seedProjectForIngest(t, pool)
			s := serverForDB(pool)

			if tc.breakDB {
				// Drop the issues table AFTER the project (which auth reads) is
				// seeded: GetProjectByPublicKey still succeeds so auth passes, but
				// UpsertIssue then fails with a real DB error inside ingestOne.
				if _, err := pool.Exec(context.Background(), "DROP TABLE issues CASCADE"); err != nil {
					t.Fatalf("drop issues: %v", err)
				}
			}

			rec := storeEvent(s, proj.PublicKey, tc.body)
			s.bgWG.Wait()
			if rec.Code != tc.wantCode {
				t.Fatalf("%s: want %d, got %d (%s)", tc.name, tc.wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHeartbeatMarkerFailureRollsBackEvent proves that the release freshness
// marker and its heartbeat event share one database transaction. If the marker
// cannot be updated, the caller receives a retryable response and the event is
// not left behind to be duplicated by the retry.
func TestHeartbeatMarkerFailureRollsBackEvent(t *testing.T) {
	pool := newTestDB(t)
	proj := seedProjectForIngest(t, pool)
	s := serverForDB(pool)

	// The event path can authenticate and resolve the project without this
	// table. Dropping it makes TouchReleaseHeartbeat fail after InsertEvent has
	// run inside the transaction.
	if _, err := pool.Exec(context.Background(), "DROP TABLE releases"); err != nil {
		t.Fatalf("drop releases: %v", err)
	}

	body := `{"level":"info","message":"service-up:web","release":"v1"}`
	rec := storeEvent(s, proj.PublicKey, body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when heartbeat marker cannot be persisted, got %d (%s)", rec.Code, rec.Body.String())
	}
	s.bgWG.Wait()
	if got := countEvents(t, pool, proj.ID); got != 0 {
		t.Fatalf("heartbeat event remained after marker failure: got %d rows", got)
	}
}
