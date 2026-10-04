package connectorsync

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultSchedulerInterval = 5 * time.Minute
	defaultSchedulerLimit    = 20
	maxSchedulerLimit        = 100
	defaultSchedulerTimeout  = 30 * time.Second
)

// DueConnectionLister is the server-owned due-list query. It returns metadata
// only; credentials and provider payloads are intentionally absent.
type DueConnectionLister interface {
	ListDueHubSpotConnectorConnections(context.Context, generated.ListDueHubSpotConnectorConnectionsParams) ([]*generated.ListDueHubSpotConnectorConnectionsRow, error)
}

// ScheduledWorker is the due-gated provider boundary. Implementations must
// claim the lease atomically before reading credentials or calling a provider.
type ScheduledWorker interface {
	RunScheduled(context.Context, string, string, string) (RunResult, error)
}

// Scheduler runs bounded due connector work. It never creates one goroutine per
// installation and it has no provider credential access of its own: the worker
// owns that boundary. A deployment must explicitly enable the caller's Run
// loop; RunOnce is also safe for an external scheduler.
type Scheduler struct {
	Lister       DueConnectionLister
	Worker       ScheduledWorker
	Now          func() time.Time
	Limit        int32
	RunTimeout   time.Duration
	WorkerID     func() string
	TickInFlight atomic.Bool
}

type SchedulerRun struct {
	Considered int
	Claimed    int
	Skipped    int
	Failed     int
}

func NewScheduler(lister DueConnectionLister, worker ScheduledWorker) *Scheduler {
	return &Scheduler{Lister: lister, Worker: worker, Now: time.Now, Limit: defaultSchedulerLimit, RunTimeout: defaultSchedulerTimeout}
}

// RunOnce lists a bounded batch and executes each provider pass sequentially.
// Sequential dispatch keeps one tick's outbound concurrency bounded; each pass
// has its own deadline and the database lease prevents duplicate work across
// scheduler replicas. A lease race is a normal skip, not an infrastructure
// failure.
func (s *Scheduler) RunOnce(ctx context.Context) (SchedulerRun, error) {
	if s == nil || s.Lister == nil || s.Worker == nil {
		return SchedulerRun{}, errors.New("connector scheduler is unavailable")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	limit := s.Limit
	if limit <= 0 {
		limit = defaultSchedulerLimit
	}
	if limit > maxSchedulerLimit {
		limit = maxSchedulerLimit
	}
	rows, err := s.Lister.ListDueHubSpotConnectorConnections(ctx, generated.ListDueHubSpotConnectorConnectionsParams{
		Now:        pgtype.Timestamptz{Time: now, Valid: true},
		BatchLimit: limit,
	})
	if err != nil {
		return SchedulerRun{}, err
	}
	result := SchedulerRun{}
	for _, row := range rows {
		if row == nil {
			continue
		}
		result.Considered++
		// The query is provider-specific today. Keep this check at the execution
		// boundary as a defense against a future query broadening accidentally.
		if row.Provider != "hubspot" {
			result.Skipped++
			continue
		}
		workerID := "scheduler:" + id.New()
		if s.WorkerID != nil {
			workerID = s.WorkerID()
		}
		timeout := s.RunTimeout
		if timeout <= 0 {
			timeout = defaultSchedulerTimeout
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		run, runErr := s.Worker.RunScheduled(runCtx, row.OrgID, row.ConnectionID, workerID)
		cancel()
		if runErr != nil {
			if errors.Is(runErr, pgx.ErrNoRows) {
				result.Skipped++
				continue
			}
			result.Failed++
			slog.Warn("connector scheduler pass failed", "provider", row.Provider, "error", runErr)
			continue
		}
		if run.Skipped {
			result.Skipped++
		} else if run.Claimed {
			result.Claimed++
		}
		if run.ReceiptStatus != "" && run.ReceiptStatus != "accepted" {
			// Keep receipt persistence separate from the provider outcome. A
			// scheduler must not rerun the provider call to repair missing evidence.
			slog.Warn("connector receipt not accepted", "provider", "hubspot", "operation", HubSpotAccountDetailsOperation, "receipt_status", run.ReceiptStatus)
		}
	}
	return result, nil
}

// Run starts an opt-in ticker. Overlapping ticks are skipped and never queued;
// an external caller can use RunOnce when it owns the scheduling lifecycle.
func (s *Scheduler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultSchedulerInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	slog.Info("connector scheduler started", "interval", interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.TickInFlight.CompareAndSwap(false, true) {
				slog.Warn("connector scheduler tick skipped: previous tick still running")
				continue
			}
			go func() {
				defer s.TickInFlight.Store(false)
				defer func() {
					if r := recover(); r != nil {
						slog.Error("connector scheduler tick panicked", "panic", r)
					}
				}()
				if _, err := s.RunOnce(ctx); err != nil {
					slog.Warn("connector scheduler tick failed", "error", err)
				}
			}()
		}
	}
}
