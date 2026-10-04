package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/metrics"
)

const (
	issueEvaluationBatch       = 12
	issueEvaluationMaxAttempts = 12
	issueEvaluationLease       = 30 * time.Second
	issueEvaluationHealthCap   = 1001
)

// IssueEvaluationHealth caps each count at 1001 so readiness cannot turn a
// large incident queue into an unbounded count scan. Values of 1001 mean "at
// least 1001"; the oldest age includes expired leases awaiting reclaim.
type IssueEvaluationHealth struct {
	Pending                 int64
	ExpiredLeases           int64
	Dead                    int64
	OldestPendingAgeSeconds int64
	CountsCapped            bool
}

func (s *Server) IssueEvaluationHealth(ctx context.Context) (IssueEvaluationHealth, error) {
	if s.pool == nil {
		return IssueEvaluationHealth{}, errors.New("issue evaluation store unavailable")
	}
	var h IssueEvaluationHealth
	err := s.pool.QueryRow(ctx, `WITH pending AS MATERIALIZED (
		SELECT created_at FROM issue_alert_evaluations
		WHERE state='pending' ORDER BY created_at LIMIT $1
	), expired AS MATERIALIZED (
		SELECT created_at FROM issue_alert_evaluations
		WHERE state='leased' AND leased_until < now() ORDER BY leased_until LIMIT $1
	), dead AS MATERIALIZED (
		SELECT id FROM issue_alert_evaluations WHERE state='dead' LIMIT $1
	)
	SELECT (SELECT count(*) FROM pending), (SELECT count(*) FROM expired),
	       (SELECT count(*) FROM dead),
	       COALESCE(EXTRACT(EPOCH FROM now() - LEAST(
	           (SELECT min(created_at) FROM pending),
	           (SELECT min(created_at) FROM expired))), 0)::bigint`,
		issueEvaluationHealthCap).Scan(&h.Pending, &h.ExpiredLeases, &h.Dead, &h.OldestPendingAgeSeconds)
	if err != nil {
		return IssueEvaluationHealth{}, fmt.Errorf("query issue evaluation health: %w", err)
	}
	h.CountsCapped = h.Pending >= issueEvaluationHealthCap ||
		h.ExpiredLeases >= issueEvaluationHealthCap || h.Dead >= issueEvaluationHealthCap
	return h, nil
}

type issueEvaluation struct {
	ID, OrgID, ProjectID, IssueID string
	IsNew, Reopened               bool
	EventCount                    int64
	ObservedAt                    time.Time
	Attempts                      int
	LeaseToken                    string
}

// RunIssueAlertEvaluations drains jobs atomically committed with ingested
// events. Every replica may run it: SKIP LOCKED and fenced leases make a
// crashed worker's pending work available to another replica.
func (s *Server) RunIssueAlertEvaluations(ctx context.Context) {
	if s.pool == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	pruneTicker := time.NewTicker(time.Minute)
	defer pruneTicker.Stop()
	for {
		s.issueEvaluationTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-pruneTicker.C:
			pruneCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := s.pruneIssueEvaluations(pruneCtx); err != nil && ctx.Err() == nil {
				slog.Warn("prune completed issue evaluations", "error", err)
			}
			cancel()
		case <-ticker.C:
		}
	}
}

func (s *Server) pruneIssueEvaluations(ctx context.Context) error {
	// Completed handoffs need only a short audit window. Failed/dead jobs stay
	// visible for operator action, and this bounded sweep cannot monopolize DB.
	_, err := s.pool.Exec(ctx, `WITH old AS (
		SELECT id FROM issue_alert_evaluations
		WHERE state='done' AND updated_at < now()-interval '7 days'
		ORDER BY updated_at,id LIMIT 10000 FOR UPDATE SKIP LOCKED
	)
	DELETE FROM issue_alert_evaluations j USING old WHERE j.id=old.id`)
	return err
}

func (s *Server) issueEvaluationTick(ctx context.Context) {
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	jobs, err := s.claimIssueEvaluations(claimCtx)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("claim issue alert evaluations", "error", err)
		}
		return
	}
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(job issueEvaluation) {
			defer wg.Done()
			s.runIssueEvaluation(ctx, job)
		}(job)
	}
	wg.Wait()
}

func (s *Server) claimIssueEvaluations(ctx context.Context) ([]issueEvaluation, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE issue_alert_evaluations
		SET state='dead', lease_token='', leased_until=NULL, updated_at=now()
		WHERE state='leased' AND leased_until < now() AND attempts >= $1`, issueEvaluationMaxAttempts); err != nil {
		return nil, fmt.Errorf("expire exhausted issue evaluation leases: %w", err)
	}
	token := id.New()
	rows, err := s.pool.Query(ctx, `WITH due AS (
		SELECT id FROM issue_alert_evaluations
		WHERE (state='pending' AND next_attempt_at <= now())
		   OR (state='leased' AND leased_until < now() AND attempts < $1)
		ORDER BY next_attempt_at, id FOR UPDATE SKIP LOCKED LIMIT $2
	)
	UPDATE issue_alert_evaluations j
	SET state='leased', attempts=j.attempts+1, lease_token=$3,
	    leased_until=now()+($4::bigint * interval '1 microsecond'), updated_at=now()
	FROM due WHERE j.id=due.id
	RETURNING j.id,j.org_id,j.project_id,j.issue_id,j.is_new,j.reopened,
	          j.event_count,j.observed_at,j.attempts,j.lease_token`,
		issueEvaluationMaxAttempts, issueEvaluationBatch, token, issueEvaluationLease.Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []issueEvaluation
	for rows.Next() {
		var j issueEvaluation
		if err := rows.Scan(&j.ID, &j.OrgID, &j.ProjectID, &j.IssueID, &j.IsNew, &j.Reopened,
			&j.EventCount, &j.ObservedAt, &j.Attempts, &j.LeaseToken); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *Server) runIssueEvaluation(parent context.Context, j issueEvaluation) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("issue alert evaluation panicked", "job_id", j.ID, "panic", r)
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
			defer cancel()
			_ = s.finishIssueEvaluation(finishCtx, j, false)
		}
	}()
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	project, err := s.q.GetProjectByID(ctx, generated.GetProjectByIDParams{ID: j.ProjectID, OrgScope: j.OrgID})
	if err == nil {
		var issue *generated.Issue
		issue, err = s.q.GetIssue(ctx, generated.GetIssueParams{ID: j.IssueID, OrgID: j.OrgID})
		if err == nil && issue.ProjectID != j.ProjectID {
			err = errors.New("issue evaluation project scope mismatch")
		}
		if err == nil {
			snapshot := &generated.UpsertIssueRow{
				ID: issue.ID, ProjectID: issue.ProjectID, OrgID: issue.OrgID,
				Title: issue.Title, Culprit: issue.Culprit, Level: issue.Level,
				Status: issue.Status, EventCount: j.EventCount,
			}
			err = s.evaluateAlerts(ctx, project, snapshot, j.IsNew, j.Reopened, j.ID, j.LeaseToken, j.ObservedAt)
		}
	}
	// A project/issue deleted after claim also cascades the job. Treat that as
	// completed work; a transient query error remains retryable.
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer finishCancel()
	if finishErr := s.finishIssueEvaluation(finishCtx, j, err == nil); finishErr != nil {
		slog.Warn("finish issue alert evaluation", "job_id", j.ID, "error", finishErr)
		metrics.Inc("flare_issue_evaluation_finishes", map[string]string{"outcome": "failed"})
		return
	}
	if err != nil {
		slog.Warn("issue alert evaluation retry scheduled", "job_id", j.ID, "attempt", j.Attempts, "error", err)
		metrics.Inc("flare_issue_evaluation_finishes", map[string]string{"outcome": "retry"})
	} else {
		metrics.Inc("flare_issue_evaluation_finishes", map[string]string{"outcome": "done"})
	}
}

func (s *Server) finishIssueEvaluation(ctx context.Context, j issueEvaluation, ok bool) error {
	state := "pending"
	if ok {
		state = "done"
	} else if j.Attempts >= issueEvaluationMaxAttempts {
		state = "dead"
	}
	backoff := time.Duration(1<<min(j.Attempts, 8)) * time.Second
	result, err := s.pool.Exec(ctx, `UPDATE issue_alert_evaluations
		SET state=$5, lease_token='', leased_until=NULL,
		    next_attempt_at=CASE WHEN $5='pending' THEN now()+($6::bigint * interval '1 microsecond') ELSE next_attempt_at END,
		    updated_at=now()
		WHERE id=$1 AND org_id=$2 AND project_id=$3 AND lease_token=$4
		  AND state='leased' AND leased_until > now()`,
		j.ID, j.OrgID, j.ProjectID, j.LeaseToken, state, backoff.Microseconds())
	if err != nil {
		return err
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	// dispatchWithAutomationClaim may have marked the job done in the same
	// transaction as its automation notices. That is the successful case.
	var current string
	err = s.pool.QueryRow(ctx, `SELECT state FROM issue_alert_evaluations
		WHERE id=$1 AND org_id=$2 AND project_id=$3`, j.ID, j.OrgID, j.ProjectID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && current == "done" && ok) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("stale issue alert evaluation lease: %s", j.ID)
}

func completeIssueEvaluationTx(ctx context.Context, tx pgx.Tx, evaluationID, org, project, leaseToken string) error {
	if evaluationID == "" {
		return nil
	}
	result, err := tx.Exec(ctx, `UPDATE issue_alert_evaluations
		SET state='done', lease_token='', leased_until=NULL, updated_at=now()
		WHERE id=$1 AND org_id=$2 AND project_id=$3 AND lease_token=$4
		  AND state='leased' AND leased_until > now()`,
		evaluationID, org, project, leaseToken)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("stale issue evaluation lease: %s", evaluationID)
	}
	return nil
}
