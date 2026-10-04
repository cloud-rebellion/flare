package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/ratelimit"
)

func TestNormalizeBusinessCheckRequest(t *testing.T) {
	got, err := normalizeBusinessCheckRequest(businessCheckRequest{
		CheckName: " Checkout.Completed ", Outcome: "OK", ElapsedMS: businessElapsed(42),
		StatusClass: "5xx", ErrorCode: "ignored",
	})
	if err != nil {
		t.Fatalf("normalize valid success: %v", err)
	}
	if got.CheckName != "checkout.completed" || got.Outcome != "ok" || got.StatusClass != "2xx" || got.ErrorCode != "" {
		t.Fatalf("normalized success = %#v", got)
	}
	for _, tc := range []struct {
		name string
		in   businessCheckRequest
	}{
		{"missing name", businessCheckRequest{Outcome: "ok"}},
		{"unsafe name", businessCheckRequest{CheckName: "checkout?user", Outcome: "ok"}},
		{"unknown outcome", businessCheckRequest{CheckName: "checkout", Outcome: "partial"}},
		{"missing elapsed", businessCheckRequest{CheckName: "checkout", Outcome: "ok"}},
		{"bad status", businessCheckRequest{CheckName: "checkout", Outcome: "error", StatusClass: "503"}},
		{"error without reason", businessCheckRequest{CheckName: "checkout", Outcome: "error"}},
		{"negative elapsed", businessCheckRequest{CheckName: "checkout", Outcome: "ok", ElapsedMS: businessElapsed(-1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := normalizeBusinessCheckRequest(tc.in); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func businessElapsed(ms int64) *int64 { return &ms }

func TestBusinessCheckIdempotencyHashes(t *testing.T) {
	for _, raw := range []string{"", "short", strings.Repeat("x", 129), "contains space", "contains:colon"} {
		r := httptest.NewRequest(http.MethodPost, "/api/p/business-checks", nil)
		r.Header.Set("Idempotency-Key", raw)
		if _, err := businessIdempotencyKeyHash(r); err == nil {
			t.Fatalf("accepted invalid key %q", raw)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/api/p/business-checks", nil)
	r.Header.Add("Idempotency-Key", "unique-request-0001")
	r.Header.Add("Idempotency-Key", "unique-request-0002")
	if _, err := businessIdempotencyKeyHash(r); err == nil {
		t.Fatal("accepted ambiguous multiple keys")
	}
	r.Header.Del("Idempotency-Key")
	r.Header.Set("Idempotency-Key", "unique-request-0001")
	hash, err := businessIdempotencyKeyHash(r)
	if err != nil || len(hash) != 64 || strings.Contains(hash, "unique-request") {
		t.Fatalf("key hash = %q (%v)", hash, err)
	}
	first, err := normalizeBusinessCheckRequest(businessCheckRequest{
		CheckName: " Checkout.Completed ", Outcome: "OK", ElapsedMS: businessElapsed(42), StatusClass: "5xx", ErrorCode: "ignored",
	})
	if err != nil {
		t.Fatal(err)
	}
	equivalent, err := normalizeBusinessCheckRequest(businessCheckRequest{
		CheckName: "checkout.completed", Outcome: "ok", ElapsedMS: businessElapsed(42), StatusClass: "2xx",
	})
	if err != nil {
		t.Fatal(err)
	}
	if businessPayloadHash(first) != businessPayloadHash(equivalent) {
		t.Fatal("normalized equivalent payloads produced different hashes")
	}
	equivalent.ElapsedMS = businessElapsed(43)
	if businessPayloadHash(first) == businessPayloadHash(equivalent) {
		t.Fatal("changed elapsed_ms produced same payload hash")
	}
}

func TestNormalizeBusinessHealth(t *testing.T) {
	threshold := 0.25
	name, comparison, value, err := normalizeBusinessHealth(" checkout.completed ", "GTE", &threshold, 15)
	if err != nil || name != "checkout.completed" || comparison != "gte" || value != threshold {
		t.Fatalf("normalized rule = %q/%q/%.2f (%v)", name, comparison, value, err)
	}
	for _, tc := range []struct {
		name      string
		checkName string
		compare   string
		threshold float64
		window    int32
	}{
		{"missing name", "", "gte", .2, 15},
		{"unsafe name", "checkout?x", "gte", .2, 15},
		{"bad comparison", "checkout", "lte", .2, 15},
		{"negative threshold", "checkout", "gte", -.1, 15},
		{"over one threshold", "checkout", "gte", 1.1, 15},
		{"too wide window", "checkout", "gte", .2, 721},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := normalizeBusinessHealth(tc.checkName, tc.compare, &tc.threshold, tc.window); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestBusinessHealthReason(t *testing.T) {
	stats := &generated.GetBusinessHealthStatsRow{Attempts: 10, ErrorCount: 3}
	if got := businessHealthReason(stats, "checkout.completed", "gte", .3, 15); got == "" {
		t.Fatal("expected threshold to fire")
	}
	if got := businessHealthReason(stats, "checkout.completed", "gt", .3, 15); got != "" {
		t.Fatal("strict threshold should not fire at equality")
	}
	if got := businessHealthReason(stats, "checkout.completed", "gte", .4, 15); got != "" {
		t.Fatal("below threshold should not fire")
	}
	if got := businessHealthReason(&generated.GetBusinessHealthStatsRow{}, "checkout.completed", "gte", 0, 15); got != "" {
		t.Fatal("empty business series must not fire")
	}
	if got := businessHealthReason(&generated.GetBusinessHealthStatsRow{Attempts: 10}, "checkout.completed", "gte", 0, 15); got != "" {
		t.Fatal("zero error threshold must stay quiet for an all-success series")
	}
	if got := businessHealthReason(&generated.GetBusinessHealthStatsRow{Attempts: 10, ErrorCount: 1}, "checkout.completed", "gte", 0, 15); got == "" {
		t.Fatal("zero threshold must fire once an error is observed")
	}
}

func TestBusinessHealthMapperIsPayloadFree(t *testing.T) {
	out := businessHealthResponse{CheckName: "checkout.completed", Attempts: 2, OKCount: 1, ErrorCount: 1, ErrorRate: .5}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, `"check_name":"checkout.completed"`) || !strings.Contains(text, `"error_rate":0.5`) {
		t.Fatalf("response missing fields: %s", text)
	}
	for _, forbidden := range []string{"org_id", "project_id", "url", "token", "payload", "customer"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("business response leaked %q: %s", forbidden, text)
		}
	}
}

func TestBusinessHealthRoutesAreSeparateFromPublicDSNIngest(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, `r.With(s.rateLimitBusinessCheck).Post("/{projectID}/business-checks", s.handleBusinessCheck)`) {
		t.Fatal("business check route is missing its separate bearer-key limiter")
	}
	if strings.Contains(body, `r.Post("/{projectID}/business-checks", s.handleBusinessCheck)`) {
		t.Fatal("business check route was left in the public DSN ingest group")
	}
	if !strings.Contains(body, `r.Get("/projects/{id}/business-health", s.handleListBusinessHealth)`) {
		t.Fatal("business health route is missing from authenticated viewer group")
	}
	if got := canonicalReceiptPillar("business"); got != "business" {
		t.Fatalf("receipt pillar = %q", got)
	}
}

func TestBusinessRateLimitChargesIPBeforeRotatedUnknownTokens(t *testing.T) {
	s := &Server{ingestLimiter: ratelimit.New(2, time.Minute)}
	calls := 0
	handler := s.rateLimitBusinessCheck(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusAccepted)
	}))
	for i, token := range []string{
		"flb_" + strings.Repeat("a", 48),
		"flb_" + strings.Repeat("b", 48),
		"flb_" + strings.Repeat("c", 48),
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/p/business-checks", nil)
		r.RemoteAddr = "192.0.2.10:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Flare-Key", "public-dsn-"+string(rune('a'+i)))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if i < 2 && w.Code != http.StatusAccepted || i == 2 && w.Code != http.StatusTooManyRequests {
			t.Fatalf("rotated bearer attempt %d = %d, calls %d", i, w.Code, calls)
		}
	}
	if calls != 2 {
		t.Fatalf("unknown bearer rotation bypassed IP limit: %d accepted", calls)
	}
}

func TestBusinessRateLimitAlsoChargesBearerAcrossIPs(t *testing.T) {
	s := &Server{ingestLimiter: ratelimit.New(1, time.Minute)}
	handler := s.rateLimitBusinessCheck(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	for i, ip := range []string{"192.0.2.10:1234", "192.0.2.11:1234"} {
		r := httptest.NewRequest(http.MethodPost, "/api/p/business-checks", nil)
		r.RemoteAddr = ip
		r.Header.Set("Authorization", "Bearer flb_"+strings.Repeat("a", 48))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if i == 0 && w.Code != http.StatusAccepted || i == 1 && w.Code != http.StatusTooManyRequests {
			t.Fatalf("shared bearer from IP %d = %d", i, w.Code)
		}
	}
}
