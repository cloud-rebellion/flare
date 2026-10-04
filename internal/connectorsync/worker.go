package connectorsync

// This file contains the first provider worker built on top of the generic
// connector sync lease. It intentionally performs a bounded HubSpot account
// details check only. It does not read or mutate CRM records, and it is not
// started by the Flare HTTP process unless the explicit connector scheduler
// setting is enabled. Manual and scheduled calls share the same durable,
// tenant-scoped execution boundary.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/hubspot"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/secretbox"
)

const (
	// HubSpotAccountDetailsOperation is deliberately a health/synchronization
	// operation with no CRM record payload. A future records adapter must use a
	// new operation and its own bounded cursor contract.
	HubSpotAccountDetailsOperation = "account_details"

	defaultHubSpotLease       = 30 * time.Second
	defaultHubSpotRunTimeout  = 20 * time.Second
	defaultHubSpotSuccessWait = 15 * time.Minute
	defaultHubSpotErrorWait   = 5 * time.Minute
	maxHubSpotRetryWait       = time.Hour
	workerCompletionTimeout   = 5 * time.Second
)

// StateStore is the durable lease boundary. Manager implements it; keeping
// this interface small lets the worker be tested without a database.
type StateStore interface {
	Ensure(context.Context, string, string, string, string) (State, error)
	Claim(context.Context, string, string, string, string, time.Duration) (State, error)
	Complete(context.Context, string, string, string, string, string, string, *string, *int64, *time.Time) (State, error)
}

// DueStateStore is implemented by the production state manager for scheduled
// work. Keeping it separate preserves the manual worker contract and makes it
// impossible for a scheduler to accidentally force an early run through the
// ordinary Claim method.
type DueStateStore interface {
	ClaimDue(context.Context, string, string, string, string, time.Duration) (State, error)
}

// Connection is the metadata needed before an outbound provider call. It has
// no credential material.
type Connection struct {
	ID                   string
	OrgID                string
	ProjectID            string
	InstallationID       string
	Provider             string
	Status               string
	CredentialConfigured bool
}

// ConnectionStore separates tenant-bound metadata and secret reads from the
// provider worker. Implementations must enforce both org_id and connection_id
// in every query.
type ConnectionStore interface {
	Get(context.Context, string, string) (Connection, error)
	Secret(context.Context, string, string) (string, error)
}

// SQLConnectionStore is the production adapter for the connector registry.
// Secret is read only after the lease has been acquired.
type SQLConnectionStore struct{ Q *generated.Queries }

func (s SQLConnectionStore) Get(ctx context.Context, orgID, connectionID string) (Connection, error) {
	if s.Q == nil {
		return Connection{}, errors.New("connector connection store is unavailable")
	}
	row, err := s.Q.GetConnectorConnectionByID(ctx, generated.GetConnectorConnectionByIDParams{ID: connectionID, OrgID: orgID})
	if err != nil {
		return Connection{}, err
	}
	return Connection{
		ID: row.ID, OrgID: row.OrgID, ProjectID: row.ProjectID.String, InstallationID: row.InstallationID, Provider: row.Provider, Status: row.Status,
		CredentialConfigured: row.CredentialConfigured,
	}, nil
}

func (s SQLConnectionStore) Secret(ctx context.Context, orgID, connectionID string) (string, error) {
	if s.Q == nil {
		return "", errors.New("connector connection store is unavailable")
	}
	return s.Q.GetConnectorConnectionSecret(ctx, generated.GetConnectorConnectionSecretParams{ID: connectionID, OrgID: orgID})
}

// HubSpotProbe is the safe outbound adapter. hubspot.Client implements it.
type HubSpotProbe interface {
	Check(context.Context, string) (hubspot.Result, error)
}

// HubSpotWorker executes one account-details health sync under a durable
// connector lease. It never creates a ticker or goroutine; the API or the
// opt-in scheduler owns invocation cadence.
type HubSpotWorker struct {
	States      StateStore
	Connections ConnectionStore
	Cipher      *secretbox.Cipher
	Probe       HubSpotProbe
	// ResolveToken refreshes OAuth grants before a probe. The legacy encrypted
	// static-token path below remains available for manual installations.
	ResolveToken func(context.Context, string, string) (string, error)

	// These fields are overridable in tests. Production uses the defaults.
	Now             func() time.Time
	LeaseFor        time.Duration
	RunTimeout      time.Duration
	SuccessInterval time.Duration
	ErrorInterval   time.Duration
}

func NewHubSpotWorker(q *generated.Queries, cipher *secretbox.Cipher, probe HubSpotProbe) *HubSpotWorker {
	if probe == nil {
		probe = hubspot.New()
	}
	return &HubSpotWorker{
		States: New(q, cipher), Connections: SQLConnectionStore{Q: q}, Cipher: cipher, Probe: probe,
		Now: time.Now, LeaseFor: defaultHubSpotLease, RunTimeout: defaultHubSpotRunTimeout,
		SuccessInterval: defaultHubSpotSuccessWait, ErrorInterval: defaultHubSpotErrorWait,
	}
}

// RunResult is safe to return to a scheduler or dashboard. It never contains
// provider response text, credentials, cursor bytes, or tenant identifiers.
type RunResult struct {
	State       State
	Outcome     string
	StatusClass string
	ErrorCode   string
	// ReceiptStatus reports the separate connector-attempt persistence result.
	// A failed receipt never changes the provider outcome or repeats the probe.
	ReceiptStatus string
	Claimed       bool
	Skipped       bool
	SkipReason    string
}

// Run performs one bounded HubSpot account-details sync. A busy lease is a
// normal skipped result. Provider failures are recorded and completed as a
// failed state; the returned error is reserved for infrastructure failures so
// callers do not accidentally log provider response details.
func (w *HubSpotWorker) Run(ctx context.Context, orgID, connectionID, workerID string) (RunResult, error) {
	return w.run(ctx, orgID, connectionID, workerID, false)
}

// RunScheduled performs one due-gated sync. The state store must implement
// DueStateStore; the atomic database claim enforces next_run_at and lease
// ownership so a stale due-list result cannot trigger an early provider call.
func (w *HubSpotWorker) RunScheduled(ctx context.Context, orgID, connectionID, workerID string) (RunResult, error) {
	return w.run(ctx, orgID, connectionID, workerID, true)
}

func (w *HubSpotWorker) run(ctx context.Context, orgID, connectionID, workerID string, scheduled bool) (RunResult, error) {
	if w == nil || w.States == nil || w.Connections == nil {
		return RunResult{}, errors.New("hubspot sync worker is unavailable")
	}
	if orgID == "" || connectionID == "" {
		return RunResult{}, errors.New("hubspot sync scope is invalid")
	}
	conn, err := w.Connections.Get(ctx, orgID, connectionID)
	if err != nil {
		return RunResult{}, err
	}
	if conn.OrgID != orgID || conn.ID != connectionID {
		return RunResult{}, errors.New("connector connection scope mismatch")
	}
	if conn.Provider != "hubspot" {
		return RunResult{Skipped: true, SkipReason: "unsupported_provider"}, nil
	}
	if conn.Status != "active" {
		return RunResult{Skipped: true, SkipReason: "connection_disabled"}, nil
	}
	if !conn.CredentialConfigured {
		return RunResult{Skipped: true, SkipReason: "credential_missing"}, nil
	}

	operation := HubSpotAccountDetailsOperation
	state, err := w.States.Ensure(ctx, orgID, connectionID, operation, id.New())
	if err != nil {
		return RunResult{}, err
	}
	leaseFor := w.LeaseFor
	if leaseFor <= 0 {
		leaseFor = defaultHubSpotLease
	}
	var claimed State
	if scheduled {
		dueStore, ok := w.States.(DueStateStore)
		if !ok {
			return RunResult{}, errors.New("scheduled connector sync is unavailable")
		}
		claimed, err = dueStore.ClaimDue(ctx, orgID, connectionID, operation, workerID, leaseFor)
	} else {
		claimed, err = w.States.Claim(ctx, orgID, connectionID, operation, workerID, leaseFor)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return RunResult{State: state, Skipped: true, SkipReason: "lease_busy"}, nil
	}
	if err != nil {
		return RunResult{}, err
	}

	result := RunResult{State: claimed, Claimed: true}
	runCtx := ctx
	runTimeout := w.RunTimeout
	if runTimeout <= 0 {
		runTimeout = defaultHubSpotRunTimeout
	}
	var cancel context.CancelFunc
	runCtx, cancel = context.WithTimeout(ctx, runTimeout)
	defer cancel()
	scopedCtx := hubspot.WithExpectedAccountID(integration.WithScope(runCtx, integration.Scope{OrgID: orgID, ProjectID: conn.ProjectID, ConnectionID: connectionID}), conn.InstallationID)

	var token string
	if w.ResolveToken != nil {
		token, err = w.ResolveToken(runCtx, orgID, connectionID)
	} else {
		var secretCiphertext string
		secretCiphertext, err = w.Connections.Secret(runCtx, orgID, connectionID)
		if err == nil && w.Cipher != nil {
			token, err = w.Cipher.Decrypt(secretCiphertext)
		} else if err == nil {
			err = errors.New("credential cipher unavailable")
		}
	}
	if err != nil || token == "" {
		code, statusClass := "invalid_config", ""
		if observedCode, observedStatus, ok := hubspot.PreflightFailureDetails(err); ok {
			code, statusClass = observedCode, observedStatus
		}
		return w.completeFailure(ctx, result, conn.ProjectID, orgID, connectionID, workerID, code, statusClass)
	}
	probe := w.Probe
	if probe == nil {
		probe = hubspot.New()
	}
	probeResult, probeErr := probe.Check(scopedCtx, token)
	if probeResult.Outcome == "" {
		probeResult.Outcome, probeResult.StatusClass, probeResult.ErrorCode = integration.ClassifyError(probeErr)
	}
	result.Outcome, result.StatusClass, result.ErrorCode = probeResult.Outcome, probeResult.StatusClass, probeResult.ErrorCode
	result.ReceiptStatus = probeResult.ReceiptStatus
	next := w.nextRunAt(probeResult)
	completed, completeErr := w.complete(ctx, result, orgID, connectionID, workerID, probeResult.Outcome, probeResult.ErrorCode, &next)
	if completeErr != nil {
		return completed, completeErr
	}
	// probeErr intentionally does not escape: provider errors are already
	// represented by the fixed result code and durable connector receipt.
	return completed, nil
}

func (w *HubSpotWorker) completeFailure(ctx context.Context, result RunResult, projectID, orgID, connectionID, workerID, code, statusClass string) (RunResult, error) {
	result.Outcome = "error"
	result.ErrorCode = code
	result.StatusClass = statusClass
	// The probe was not reached, so emit the same payload-free connector receipt
	// shape as the provider adapter would have emitted.
	receiptErr := integration.RecordResult(integration.WithScope(ctx, integration.Scope{OrgID: orgID, ProjectID: projectID, ConnectionID: connectionID}), integration.Attempt{
		Provider: "hubspot", Operation: HubSpotAccountDetailsOperation, Channel: "worker", Outcome: "error", StatusClass: statusClass, ErrorCode: code,
		AttemptedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(),
	})
	result.ReceiptStatus = integration.ReceiptStatus(receiptErr)
	next := w.nextRunAt(hubspot.Result{Outcome: "error", ErrorCode: code})
	return w.complete(ctx, result, orgID, connectionID, workerID, "error", code, &next)
}

func (w *HubSpotWorker) complete(ctx context.Context, result RunResult, orgID, connectionID, workerID, outcome, errorCode string, next *time.Time) (RunResult, error) {
	completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerCompletionTimeout)
	defer cancel()
	state, err := w.States.Complete(completeCtx, orgID, connectionID, HubSpotAccountDetailsOperation, workerID, outcome, errorCode, nil, nil, next)
	if err != nil {
		return result, err
	}
	result.State = state
	return result, nil
}

func (w *HubSpotWorker) nextRunAt(result hubspot.Result) time.Time {
	now := time.Now().UTC()
	if w != nil && w.Now != nil {
		now = w.Now().UTC()
	}
	if result.Outcome == "ok" {
		wait := w.SuccessInterval
		if wait <= 0 {
			wait = defaultHubSpotSuccessWait
		}
		return now.Add(wait)
	}
	if result.ErrorCode == "rate_limited" && result.RetryAfterMS != nil {
		wait := time.Duration(*result.RetryAfterMS) * time.Millisecond
		if wait > 0 && wait < maxHubSpotRetryWait {
			return now.Add(wait)
		}
	}
	wait := w.ErrorInterval
	if wait <= 0 {
		wait = defaultHubSpotErrorWait
	}
	return now.Add(wait)
}
