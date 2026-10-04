package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func sendOTLPBatch(s *Server, project *generated.Project, path, contentType, key string, body []byte, handler http.HandlerFunc) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Flare-Key", project.PublicKey)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func countOTLPKeys(t *testing.T, pool *pgxpool.Pool, project *generated.Project) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM otlp_acceptances
		WHERE org_id=$1 AND project_id=$2`, project.OrgID, project.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countOTLPReceipts(t *testing.T, pool *pgxpool.Pool, project *generated.Project, pillar string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM ingest_receipts
		WHERE org_id=$1 AND project_id=$2 AND protocol='otlp' AND pillar=$3`,
		project.OrgID, project.ID, pillar).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestOTLPKeyedReplayConcurrentSignalsAndPartialSuccess(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	traceID := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16))
	spanID := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 8))
	traceBody := fmt.Sprintf(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":%q,"spanId":%q,"name":"GET /checkout"}]}]}]}`, traceID, spanID)
	// One valid and one oversized label document exercise OTLP partialSuccess.
	metricBody := `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"checkout.requests","gauge":{"dataPoints":[{"asDouble":1},{"asDouble":2,"attributes":[{"key":"large","value":{"stringValue":"` + strings.Repeat("x", maxMetricLabelBytes) + `"}}]}]}}]}]}]}`
	cases := []struct {
		name, pillar, table, path, key, body, changed string
		handler                                       http.HandlerFunc
		wantResponse                                  string
	}{
		{"logs", "logs", "logs", "/otlp/v1/logs", "otlp-logs-batch-0001",
			`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"ok"},"severityText":"INFO"}]}]}]}`,
			`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"changed"},"severityText":"INFO"}]}]}]}`,
			s.handleOTLPLogs, `{}`},
		{"traces", "spans", "spans", "/otlp/v1/traces", "otlp-traces-batch-0001", traceBody,
			strings.Replace(traceBody, "GET /checkout", "POST /checkout", 1), s.handleOTLPTraces, `{}`},
		{"metrics", "metrics", "metrics", "/otlp/v1/metrics", "otlp-metrics-batch-0001", metricBody,
			strings.Replace(metricBody, "checkout.requests", "checkout.changed", 1), s.handleOTLPMetrics, `"rejectedDataPoints":"1"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const senders = 6
			responses := make(chan *httptest.ResponseRecorder, senders)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for range senders {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					responses <- sendOTLPBatch(s, project, tc.path, "application/json", tc.key, []byte(tc.body), tc.handler)
				}()
			}
			close(start)
			wg.Wait()
			close(responses)
			newCount := 0
			var firstBody string
			var firstReceipt string
			for response := range responses {
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), tc.wantResponse) {
					t.Fatalf("response = %d %s", response.Code, response.Body.String())
				}
				if firstBody == "" {
					firstBody = response.Body.String()
				} else if response.Body.String() != firstBody {
					t.Fatalf("replay changed OTLP response: %s versus %s", response.Body.String(), firstBody)
				}
				receiptID := response.Header().Get("X-Flare-Receipt-ID")
				if receiptID == "" {
					t.Fatal("OTLP 200 missing durable receipt ID")
				}
				if firstReceipt == "" {
					firstReceipt = receiptID
				} else if receiptID != firstReceipt {
					t.Fatalf("replay receipt %q differs from %q", receiptID, firstReceipt)
				}
				if response.Header().Get("Idempotency-Replayed") != "true" {
					newCount++
				}
			}
			if newCount != 1 || otlpRowCount(t, pool, tc.table, project) != 1 || countOTLPReceipts(t, pool, project, tc.pillar) != 1 {
				t.Fatalf("%s durable acceptance = %d new, %d rows, %d receipts; want 1 each",
					tc.name, newCount, otlpRowCount(t, pool, tc.table, project), countOTLPReceipts(t, pool, project, tc.pillar))
			}
			changed := sendOTLPBatch(s, project, tc.path, "application/json", tc.key, []byte(tc.changed), tc.handler)
			if changed.Code != http.StatusConflict || otlpRowCount(t, pool, tc.table, project) != 1 || countOTLPReceipts(t, pool, project, tc.pillar) != 1 {
				t.Fatalf("changed-body conflict = %d, rows=%d receipts=%d", changed.Code,
					otlpRowCount(t, pool, tc.table, project), countOTLPReceipts(t, pool, project, tc.pillar))
			}
		})
	}
	if got := countOTLPKeys(t, pool, project); got != 3 {
		t.Fatalf("project retry identities = %d, want 3", got)
	}
	// A key is unique across OTLP signals within one project.
	crossSignal := sendOTLPBatch(s, project, "/otlp/v1/traces", "application/json", cases[0].key, []byte(traceBody), s.handleOTLPTraces)
	if crossSignal.Code != http.StatusConflict {
		t.Fatalf("key reused across signals = %d %s", crossSignal.Code, crossSignal.Body.String())
	}
	other, err := generated.New(pool).CreateProject(context.Background(), generated.CreateProjectParams{
		ID: id.New(), OrgID: project.OrgID, Name: "other", Slug: "otlp-other-" + id.New(),
		Platform: "other", PublicKey: id.New(), DsnID: id.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	otherResponse := sendOTLPBatch(s, other, cases[0].path, "application/json", cases[0].key, []byte(cases[0].body), s.handleOTLPLogs)
	if otherResponse.Code != http.StatusOK || otherResponse.Header().Get("Idempotency-Replayed") == "true" || countOTLPKeys(t, pool, other) != 1 {
		t.Fatalf("same key in different project = %d replay=%q", otherResponse.Code, otherResponse.Header().Get("Idempotency-Replayed"))
	}
	// Standard keyless exporters retain their original behavior and can write
	// two identical batches after an ambiguous acknowledgement.
	for range 2 {
		response := sendOTLPBatch(s, project, cases[0].path, "application/json", "", []byte(cases[0].body), s.handleOTLPLogs)
		if response.Code != http.StatusOK {
			t.Fatalf("keyless OTLP = %d %s", response.Code, response.Body.String())
		}
	}
	if got := otlpRowCount(t, pool, "logs", project); got != 3 {
		t.Fatalf("keyless duplicate logs = %d, want 3", got)
	}
	if got := countOTLPKeys(t, pool, project); got != 3 {
		t.Fatalf("keyless request created %d retry identities, want 3", got)
	}
	var received, accepted, rejected, dropped int64
	var outcome string
	if err := pool.QueryRow(context.Background(), `SELECT r.received_count,r.accepted_count,r.rejected_count,r.dropped_count,r.outcome
		FROM otlp_acceptances a JOIN ingest_receipts r
		  ON r.id=a.receipt_id AND r.org_id=a.org_id AND r.project_id=a.project_id
		WHERE a.org_id=$1 AND a.project_id=$2 AND a.key_hash=$3`,
		project.OrgID, project.ID, mustOTLPTestKeyHash(t, cases[2].key)).
		Scan(&received, &accepted, &rejected, &dropped, &outcome); err != nil {
		t.Fatal(err)
	}
	if received != 2 || accepted != 1 || rejected != 1 || dropped != 0 || outcome != "partial" {
		t.Fatalf("partial metric receipt = %d/%d/%d/%d %s", received, accepted, rejected, dropped, outcome)
	}
}

func mustOTLPTestKeyHash(t *testing.T, key string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/otlp/v1/metrics", nil)
	req.Header.Set("Idempotency-Key", key)
	hash, err := nativeIngestKeyHash(req)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestOTLPKeyedProtobufRollbackExpiryAndValidation(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	protoBody, err := proto.Marshal(&colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
				Name: "protobuf.requests", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
					DataPoints: []*metricspb.NumberDataPoint{{Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: 1}}},
				}},
			}}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	key := "otlp-protobuf-batch-0001"
	send := func() *httptest.ResponseRecorder {
		return sendOTLPBatch(s, project, "/otlp/v1/metrics", "application/x-protobuf", key, protoBody, s.handleOTLPMetrics)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_otlp_keyed_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.protocol='otlp' THEN RAISE EXCEPTION 'forced receipt failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_otlp_keyed_receipt BEFORE UPDATE ON ingest_receipts
		FOR EACH ROW EXECUTE FUNCTION fail_otlp_keyed_receipt()`); err != nil {
		t.Fatal(err)
	}
	failed := send()
	if failed.Code != http.StatusServiceUnavailable || failed.Header().Get("Retry-After") != "1" ||
		otlpRowCount(t, pool, "metrics", project) != 0 || countOTLPReceipts(t, pool, project, "metrics") != 0 || countOTLPKeys(t, pool, project) != 0 {
		t.Fatalf("receipt failure left metrics/receipt/key: status=%d rows=%d receipts=%d keys=%d",
			failed.Code, otlpRowCount(t, pool, "metrics", project), countOTLPReceipts(t, pool, project, "metrics"), countOTLPKeys(t, pool, project))
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_otlp_keyed_receipt ON ingest_receipts; DROP FUNCTION fail_otlp_keyed_receipt()`); err != nil {
		t.Fatal(err)
	}
	first := send()
	replay := send()
	if first.Code != http.StatusOK || replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" ||
		otlpRowCount(t, pool, "metrics", project) != 1 || countOTLPReceipts(t, pool, project, "metrics") != 1 || countOTLPKeys(t, pool, project) != 1 {
		t.Fatalf("protobuf first/replay = %d/%d replay=%q rows=%d receipts=%d keys=%d",
			first.Code, replay.Code, replay.Header().Get("Idempotency-Replayed"), otlpRowCount(t, pool, "metrics", project),
			countOTLPReceipts(t, pool, project, "metrics"), countOTLPKeys(t, pool, project))
	}
	if otlpPayloadHash(protoBody, true) == otlpPayloadHash(protoBody, false) {
		t.Fatal("JSON and protobuf encoding used the same retry payload hash")
	}
	// Compression may change between attempts; the replay identity binds the
	// decoded bytes and protobuf encoding rather than a gzip stream's bytes.
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(protoBody); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	gzipRequest := httptest.NewRequest(http.MethodPost, "/otlp/v1/metrics", bytes.NewReader(compressed.Bytes()))
	gzipRequest.Header.Set("Content-Type", "application/x-protobuf")
	gzipRequest.Header.Set("Content-Encoding", "gzip")
	gzipRequest.Header.Set("X-Flare-Key", project.PublicKey)
	gzipRequest.Header.Set("Idempotency-Key", key)
	gzipReplay := httptest.NewRecorder()
	s.handleOTLPMetrics(gzipReplay, gzipRequest)
	if gzipReplay.Code != http.StatusOK || gzipReplay.Header().Get("Idempotency-Replayed") != "true" ||
		otlpRowCount(t, pool, "metrics", project) != 1 {
		t.Fatalf("compressed same-body retry = %d replay=%q", gzipReplay.Code, gzipReplay.Header().Get("Idempotency-Replayed"))
	}
	badKeyRequest := httptest.NewRequest(http.MethodPost, "/otlp/v1/metrics", bytes.NewReader(protoBody))
	badKeyRequest.Header.Set("Content-Type", "application/x-protobuf")
	badKeyRequest.Header.Set("X-Flare-Key", project.PublicKey)
	badKeyRequest.Header.Add("Idempotency-Key", key)
	badKeyRequest.Header.Add("Idempotency-Key", "different-valid-key-0002")
	badKey := httptest.NewRecorder()
	s.handleOTLPMetrics(badKey, badKeyRequest)
	if badKey.Code != http.StatusBadRequest || countOTLPKeys(t, pool, project) != 1 {
		t.Fatalf("ambiguous key header = %d keys=%d", badKey.Code, countOTLPKeys(t, pool, project))
	}
	// Malformed keyed bodies retain rejection receipts, without claiming a
	// retry identity that could hide a corrected request.
	malformed := sendOTLPBatch(s, project, "/otlp/v1/metrics", "application/json", "otlp-invalid-batch-0001", []byte("not-json"), s.handleOTLPMetrics)
	if malformed.Code != http.StatusBadRequest || countOTLPKeys(t, pool, project) != 1 {
		t.Fatalf("malformed keyed OTLP = %d keys=%d", malformed.Code, countOTLPKeys(t, pool, project))
	}
	if _, err := pool.Exec(ctx, `UPDATE otlp_acceptances SET expires_at=now()-interval '1 second'
		WHERE org_id=$1 AND project_id=$2`, project.OrgID, project.ID); err != nil {
		t.Fatal(err)
	}
	pruned, err := s.pruneOTLPAcceptances(ctx)
	if err != nil || pruned != 1 {
		t.Fatalf("OTLP expiry prune = %d %v, want 1", pruned, err)
	}
	second := send()
	if second.Code != http.StatusOK || second.Header().Get("Idempotency-Replayed") == "true" ||
		otlpRowCount(t, pool, "metrics", project) != 2 || countOTLPKeys(t, pool, project) != 1 {
		t.Fatalf("post-expiry reuse = %d replay=%q rows=%d keys=%d", second.Code, second.Header().Get("Idempotency-Replayed"),
			otlpRowCount(t, pool, "metrics", project), countOTLPKeys(t, pool, project))
	}
	// The request itself also expires its prior key if the sweep is behind.
	if _, err := pool.Exec(ctx, `UPDATE otlp_acceptances SET expires_at=now()-interval '1 second'
		WHERE org_id=$1 AND project_id=$2`, project.OrgID, project.ID); err != nil {
		t.Fatal(err)
	}
	third := send()
	if third.Code != http.StatusOK || third.Header().Get("Idempotency-Replayed") == "true" ||
		otlpRowCount(t, pool, "metrics", project) != 3 || countOTLPKeys(t, pool, project) != 1 {
		t.Fatalf("request-driven expiry reuse = %d replay=%q rows=%d keys=%d", third.Code,
			third.Header().Get("Idempotency-Replayed"), otlpRowCount(t, pool, "metrics", project), countOTLPKeys(t, pool, project))
	}
}
