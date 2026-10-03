package connectorsync

import (
	"context"
	"testing"
	"time"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestSchedulerRunOnceBoundsAndScopesProviderPasses(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	lister := &fakeDueLister{rows: []*generated.ListDueHubSpotConnectorConnectionsRow{
		{OrgID: "org-a", ConnectionID: "conn-a", Provider: "hubspot", NextRunAt: pgtype.Timestamptz{Time: now, Valid: true}},
		{OrgID: "org-b", ConnectionID: "conn-b", Provider: "salesforce", NextRunAt: pgtype.Timestamptz{Time: now, Valid: true}},
	}}
	worker := &fakeScheduledWorker{}
	scheduler := NewScheduler(lister, worker)
	scheduler.Now = func() time.Time { return now }
	scheduler.Limit = 1000
	scheduler.WorkerID = func() string { return "scheduler:test" }

	got, err := scheduler.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got.Considered != 2 || got.Claimed != 1 || got.Skipped != 1 || got.Failed != 0 {
		t.Fatalf("run = %#v", got)
	}
	if lister.limit != maxSchedulerLimit || !lister.at.Equal(now) {
		t.Fatalf("list args = limit %d at %v", lister.limit, lister.at)
	}
	if len(worker.calls) != 1 || worker.calls[0] != "org-a/conn-a/scheduler:test" {
		t.Fatalf("worker calls = %#v", worker.calls)
	}
}

func TestSchedulerRunOnceTreatsLeaseRaceAsSkip(t *testing.T) {
	lister := &fakeDueLister{rows: []*generated.ListDueHubSpotConnectorConnectionsRow{{OrgID: "org", ConnectionID: "conn", Provider: "hubspot"}}}
	worker := &fakeScheduledWorker{err: pgx.ErrNoRows}
	scheduler := NewScheduler(lister, worker)
	got, err := scheduler.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got.Failed != 0 || got.Skipped != 1 {
		t.Fatalf("run = %#v", got)
	}
}

type fakeDueLister struct {
	rows  []*generated.ListDueHubSpotConnectorConnectionsRow
	at    time.Time
	limit int32
}

func (f *fakeDueLister) ListDueHubSpotConnectorConnections(_ context.Context, arg generated.ListDueHubSpotConnectorConnectionsParams) ([]*generated.ListDueHubSpotConnectorConnectionsRow, error) {
	f.at, f.limit = arg.Now.Time, arg.BatchLimit
	return f.rows, nil
}

type fakeScheduledWorker struct {
	calls []string
	err   error
}

func (f *fakeScheduledWorker) RunScheduled(_ context.Context, orgID, connectionID, workerID string) (RunResult, error) {
	f.calls = append(f.calls, orgID+"/"+connectionID+"/"+workerID)
	if f.err != nil {
		return RunResult{}, f.err
	}
	return RunResult{Claimed: true}, nil
}
