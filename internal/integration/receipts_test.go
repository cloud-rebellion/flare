package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRecordDropsUnscopedAndCanonicalizes(t *testing.T) {
	previous := func(context.Context, Attempt) {}
	SetRecorder(previous)
	t.Cleanup(func() { SetRecorder(nil) })
	var got []Attempt
	SetRecorder(func(_ context.Context, a Attempt) { got = append(got, a) })
	Record(context.Background(), Attempt{Provider: "github", Operation: "create_issue", Channel: "http"})
	if len(got) != 0 {
		t.Fatal("unscoped connector attempt was recorded")
	}
	Record(WithScope(context.Background(), Scope{OrgID: "org-1", ConnectionID: "conn-1"}), Attempt{
		Provider: "github", Operation: "create_issue", Channel: "http", Outcome: "unexpected",
		Elapsed: -time.Second,
	})
	if len(got) != 1 {
		t.Fatalf("recorded %d attempts, want 1", len(got))
	}
	if got[0].Scope.ConnectionID != "conn-1" || got[0].Outcome != "error" || got[0].ErrorCode != "provider_error" || got[0].Elapsed < 0 {
		t.Fatalf("canonical attempt = %#v", got[0])
	}
}

func TestClassifyErrorNeverReturnsProviderText(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{err: context.DeadlineExceeded, want: "timeout"},
		{err: errors.New("github returned 429: token-secret and https://secret.example/x"), want: "http_4xx"},
		{err: errors.New("provider exploded with response-secret"), want: "provider_error"},
	} {
		_, _, code := ClassifyError(tc.err)
		if code != tc.want {
			t.Fatalf("ClassifyError(%v) code = %q, want %q", tc.err, code, tc.want)
		}
		if strings.Contains(code, "secret") || strings.Contains(code, "https") {
			t.Fatalf("unsafe error code %q", code)
		}
	}
}

func TestRecordBoundsProviderHints(t *testing.T) {
	SetRecorder(nil)
	t.Cleanup(func() { SetRecorder(nil) })
	var got []Attempt
	SetRecorder(func(_ context.Context, a Attempt) { got = append(got, a) })
	quota := int64(7)
	negative := int64(-1)
	tooLarge := int64(604_800_001)
	Record(WithScope(context.Background(), Scope{OrgID: "org-1"}), Attempt{
		Provider: "hubspot", Operation: "sync", Channel: "http", Outcome: "ok",
		QuotaRemaining: &quota, RetryAfterMS: &negative, SyncLagMS: &tooLarge,
	})
	if len(got) != 1 {
		t.Fatalf("recorded %d attempts, want 1", len(got))
	}
	if got[0].QuotaRemaining == nil || *got[0].QuotaRemaining != quota {
		t.Fatalf("quota hint = %#v, want %d", got[0].QuotaRemaining, quota)
	}
	if got[0].RetryAfterMS != nil || got[0].SyncLagMS != nil {
		t.Fatalf("out-of-range hints were retained: retry=%#v sync=%#v", got[0].RetryAfterMS, got[0].SyncLagMS)
	}
}

func TestRecordResultReportsPersistenceFailureWithoutProviderData(t *testing.T) {
	wantErr := errors.New("database temporarily unavailable: bearer-secret")
	SetRecorderWithError(func(_ context.Context, a Attempt) error {
		if a.Scope.OrgID != "org-1" || a.Provider != "github" || a.Operation != "create_issue" {
			t.Fatalf("recorder received unexpected canonical attempt: %#v", a)
		}
		return wantErr
	})
	t.Cleanup(func() { SetRecorder(nil) })
	err := RecordResult(WithScope(context.Background(), Scope{OrgID: "org-1"}), Attempt{
		Provider: "github", Operation: "create_issue", Channel: "http", Outcome: "ok",
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RecordResult error = %v, want %v", err, wantErr)
	}
}

func TestRecordResultRejectsMissingProvenanceAndRecorder(t *testing.T) {
	SetRecorder(nil)
	t.Cleanup(func() { SetRecorder(nil) })
	if !errors.Is(RecordResult(context.Background(), Attempt{Provider: "github", Operation: "create_issue", Channel: "http"}), ErrUnscopedAttempt) {
		t.Fatal("unscoped attempt was not rejected by RecordResult")
	}
	if !errors.Is(RecordResult(WithScope(context.Background(), Scope{OrgID: "org-1"}), Attempt{
		Provider: "github", Operation: "create_issue", Channel: "http",
	}), ErrRecorderUnavailable) {
		t.Fatal("missing recorder was not reported by RecordResult")
	}
}
