package connectorsync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bright-interaction/flare/internal/hubspot"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/secretbox"
)

func TestHubSpotWorkerClaimsChecksAndCompletesWithoutPayload(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	states := &fakeStateStore{claimed: State{ID: "sync-1", OrgID: "org-1", ConnectionID: "conn-1", Operation: HubSpotAccountDetailsOperation, Status: "running"}}
	connections := &fakeConnectionStore{
		connection: Connection{ID: "conn-1", OrgID: "org-1", ProjectID: "project-1", Provider: "hubspot", Status: "active", CredentialConfigured: true},
		secret:     "hs-token",
	}
	probe := &fakeHubSpotProbe{result: hubspot.Result{Outcome: "ok", StatusClass: "2xx", ReceiptStatus: "accepted"}}
	worker := &HubSpotWorker{
		States: states, Connections: connections, Cipher: secretbox.New(""), Probe: probe,
		Now: func() time.Time { return now }, LeaseFor: 10 * time.Second,
		RunTimeout: time.Second, SuccessInterval: 15 * time.Minute, ErrorInterval: time.Minute,
	}

	var receipt integration.Attempt
	integration.SetRecorder(func(_ context.Context, attempt integration.Attempt) { receipt = attempt })
	defer integration.SetRecorder(nil)
	got, err := worker.Run(context.Background(), "org-1", "conn-1", "worker-a")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !got.Claimed || got.Skipped || got.Outcome != "ok" || got.StatusClass != "2xx" || got.ReceiptStatus != "accepted" {
		t.Fatalf("result = %#v", got)
	}
	if states.completeOutcome != "ok" || states.completeNext == nil || !states.completeNext.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("completion = outcome %q next %v", states.completeOutcome, states.completeNext)
	}
	if probe.token != "hs-token" || connections.secretReads != 1 {
		t.Fatalf("probe/secret boundary = token %q reads %d", probe.token, connections.secretReads)
	}
	if receipt.Provider != "hubspot" || receipt.Scope.OrgID != "org-1" || receipt.Scope.ProjectID != "project-1" || receipt.Scope.ConnectionID != "conn-1" || receipt.Operation != HubSpotAccountDetailsOperation {
		t.Fatalf("receipt scope = %#v", receipt)
	}
	if strings.Contains(receipt.ErrorCode, "hs-token") {
		t.Fatal("receipt leaked token")
	}
}

func TestHubSpotWorkerSkipsBusyLeaseWithoutReadingSecret(t *testing.T) {
	states := &fakeStateStore{claimErr: pgx.ErrNoRows}
	connections := &fakeConnectionStore{connection: Connection{ID: "conn-1", OrgID: "org-1", Provider: "hubspot", Status: "active", CredentialConfigured: true}, secret: "token"}
	worker := &HubSpotWorker{States: states, Connections: connections, Cipher: secretbox.New(""), Probe: &fakeHubSpotProbe{}}
	got, err := worker.Run(context.Background(), "org-1", "conn-1", "worker-a")
	if err != nil || !got.Skipped || got.SkipReason != "lease_busy" {
		t.Fatalf("busy result = %#v err %v", got, err)
	}
	if connections.secretReads != 0 {
		t.Fatalf("secret was read while lease was busy: %d", connections.secretReads)
	}
}

func TestHubSpotWorkerScheduledUsesDueClaimBoundary(t *testing.T) {
	states := &fakeStateStore{claimed: State{ID: "sync-1", OrgID: "org-1", ConnectionID: "conn-1", Operation: HubSpotAccountDetailsOperation, Status: "running"}}
	connections := &fakeConnectionStore{
		connection: Connection{ID: "conn-1", OrgID: "org-1", Provider: "hubspot", Status: "active", CredentialConfigured: true},
		secret:     "hs-token",
	}
	worker := &HubSpotWorker{
		States: states, Connections: connections, Cipher: secretbox.New(""),
		Probe: &fakeHubSpotProbe{result: hubspot.Result{Outcome: "ok", StatusClass: "2xx"}},
		Now:   func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) },
	}
	got, err := worker.RunScheduled(context.Background(), "org-1", "conn-1", "scheduler:test")
	if err != nil || !got.Claimed || states.dueClaims != 1 || states.claims != 0 {
		t.Fatalf("scheduled result = %#v err %v due=%d manual=%d", got, err, states.dueClaims, states.claims)
	}
}

func TestHubSpotWorkerDoesNotClaimDisabledOrOtherProvider(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		status   string
		reason   string
	}{
		{name: "paused", provider: "hubspot", status: "paused", reason: "connection_disabled"},
		{name: "other provider", provider: "salesforce", status: "active", reason: "unsupported_provider"},
		{name: "missing secret", provider: "hubspot", status: "active", reason: "credential_missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states := &fakeStateStore{}
			connections := &fakeConnectionStore{connection: Connection{ID: "conn-1", OrgID: "org-1", Provider: tc.provider, Status: tc.status, CredentialConfigured: tc.reason != "credential_missing"}}
			worker := &HubSpotWorker{States: states, Connections: connections, Cipher: secretbox.New("")}
			got, err := worker.Run(context.Background(), "org-1", "conn-1", "worker-a")
			if err != nil || !got.Skipped || got.SkipReason != tc.reason {
				t.Fatalf("result = %#v err %v", got, err)
			}
			if states.claims != 0 {
				t.Fatalf("disabled connection was claimed %d times", states.claims)
			}
		})
	}
}

func TestHubSpotWorkerCompletesCredentialFailureWithBoundedCode(t *testing.T) {
	states := &fakeStateStore{claimed: State{ID: "sync-1"}}
	connections := &fakeConnectionStore{connection: Connection{ID: "conn-1", OrgID: "org-1", ProjectID: "project-1", Provider: "hubspot", Status: "active", CredentialConfigured: true}, secret: "enc:v1:broken"}
	worker := &HubSpotWorker{States: states, Connections: connections, Cipher: secretbox.New("correct-key"), Now: func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) }}
	got, err := worker.Run(context.Background(), "org-1", "conn-1", "worker-a")
	if err != nil || got.ErrorCode != "invalid_config" || states.completeOutcome != "error" || states.completeErrorCode != "invalid_config" {
		t.Fatalf("credential failure = %#v err %v complete=%q/%q", got, err, states.completeOutcome, states.completeErrorCode)
	}
}

func TestHubSpotWorkerPreflightReceiptFailureRemainsSeparate(t *testing.T) {
	states := &fakeStateStore{claimed: State{ID: "sync-1"}}
	connections := &fakeConnectionStore{connection: Connection{ID: "conn-1", OrgID: "org-1", ProjectID: "project-1", Provider: "hubspot", Status: "active", CredentialConfigured: true}, secret: "enc:v1:broken"}
	worker := &HubSpotWorker{States: states, Connections: connections, Cipher: secretbox.New("correct-key")}
	receiptCalls := 0
	integration.SetRecorderWithError(func(_ context.Context, a integration.Attempt) error {
		receiptCalls++
		if a.Scope.OrgID != "org-1" || a.Scope.ProjectID != "project-1" || a.Scope.ConnectionID != "conn-1" || a.Channel != "worker" || a.Outcome != "error" || a.ErrorCode != "invalid_config" {
			t.Fatalf("preflight receipt = %#v", a)
		}
		return errors.New("storage-secret")
	})
	t.Cleanup(func() { integration.SetRecorder(nil) })
	got, err := worker.Run(context.Background(), "org-1", "conn-1", "worker-a")
	if err != nil || got.Outcome != "error" || got.ErrorCode != "invalid_config" || got.ReceiptStatus != "unavailable" {
		t.Fatalf("provider and receipt result = %#v, %v", got, err)
	}
	if receiptCalls != 1 || states.completeOutcome != "error" || states.completeErrorCode != "invalid_config" {
		t.Fatalf("receipt calls = %d, worker completion = %q/%q", receiptCalls, states.completeOutcome, states.completeErrorCode)
	}
}

type fakeStateStore struct {
	ensured           State
	claimed           State
	claimErr          error
	completeErr       error
	completeOutcome   string
	completeErrorCode string
	completeNext      *time.Time
	claims            int
	dueClaims         int
}

func (f *fakeStateStore) Ensure(_ context.Context, _, _, _, _ string) (State, error) {
	if f.ensured.ID == "" {
		f.ensured = State{ID: "sync-1", Status: "idle"}
	}
	return f.ensured, nil
}

func (f *fakeStateStore) Claim(_ context.Context, _, _, _, _ string, _ time.Duration) (State, error) {
	f.claims++
	if f.claimErr != nil {
		return State{}, f.claimErr
	}
	if f.claimed.ID == "" {
		f.claimed = State{ID: "sync-1", Status: "running"}
	}
	return f.claimed, nil
}

func (f *fakeStateStore) ClaimDue(ctx context.Context, orgID, connectionID, operation, workerID string, leaseFor time.Duration) (State, error) {
	f.dueClaims++
	if f.claimErr != nil {
		return State{}, f.claimErr
	}
	if f.claimed.ID == "" {
		f.claimed = State{ID: "sync-1", Status: "running"}
	}
	return f.claimed, nil
}

func (f *fakeStateStore) Complete(_ context.Context, _, _, _, _, outcome, errorCode string, _ *string, _ *int64, next *time.Time) (State, error) {
	f.completeOutcome, f.completeErrorCode, f.completeNext = outcome, errorCode, next
	if f.completeErr != nil {
		return State{}, f.completeErr
	}
	state := f.claimed
	state.Status = "succeeded"
	if outcome != "ok" {
		state.Status = "failed"
	}
	return state, nil
}

type fakeConnectionStore struct {
	connection  Connection
	secret      string
	secretReads int
}

func (f *fakeConnectionStore) Get(context.Context, string, string) (Connection, error) {
	return f.connection, nil
}

func (f *fakeConnectionStore) Secret(context.Context, string, string) (string, error) {
	f.secretReads++
	return f.secret, nil
}

type fakeHubSpotProbe struct {
	result hubspot.Result
	err    error
	token  string
}

func (f *fakeHubSpotProbe) Check(ctx context.Context, token string) (hubspot.Result, error) {
	f.token = token
	// Mirror the production adapter's payload-free receipt so this test also
	// verifies that the worker attached the tenant and installation scope.
	integration.Record(ctx, integration.Attempt{Provider: "hubspot", Operation: HubSpotAccountDetailsOperation, Channel: "http", Outcome: f.result.Outcome, StatusClass: f.result.StatusClass})
	return f.result, f.err
}
