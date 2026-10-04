package api

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
)

var errMonitorLimit = errors.New("project monitor limit reached")

// reserveMonitorSlot serializes every application-level monitor creation for
// one project. The project row must be locked before counting, pruning, or
// inserting; otherwise concurrent fresh slugs can each observe 499 rows and
// both commit. An existing check-in never needs this lock.
func (s *Server) reserveMonitorSlot(ctx context.Context, tx pgx.Tx, orgID, projectID string, now time.Time) error {
	if err := s.lockMonitorProject(ctx, tx, orgID, projectID); err != nil {
		return err
	}
	return s.checkMonitorSlot(ctx, tx, orgID, projectID, now)
}

// upsertMonitorCheckinTx updates an existing slug directly, so frequent
// check-ins remain available even when the project is at capacity. A fresh
// slug takes the project lock, rechecks existence (another request may have
// created the same slug), then admits at most one new row under that lock.
func (s *Server) upsertMonitorCheckinTx(ctx context.Context, tx pgx.Tx, params generated.UpsertMonitorCheckinParams) (*generated.Monitor, error) {
	q := generated.New(tx)
	update := generated.UpdateExistingMonitorCheckinParams{
		ProjectID: params.ProjectID, OrgID: params.OrgID, Slug: params.Slug,
		LastPingAt: params.LastPingAt, LastStatus: params.LastStatus, State: params.State,
	}
	m, err := q.UpdateExistingMonitorCheckin(ctx, update)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return m, err
	}
	// The project lock stays held until commit. Recheck the slug after
	// acquiring it, before counting; a competing check-in for the same new
	// slug may already have committed.
	if err := s.lockMonitorProject(ctx, tx, params.OrgID, params.ProjectID); err != nil {
		return nil, err
	}
	m, err = q.UpdateExistingMonitorCheckin(ctx, update)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return m, err
	}
	if err := s.checkMonitorSlot(ctx, tx, params.OrgID, params.ProjectID, params.LastPingAt.Time); err != nil {
		return nil, err
	}
	return q.UpsertMonitorCheckin(ctx, params)
}

func (s *Server) lockMonitorProject(ctx context.Context, tx pgx.Tx, orgID, projectID string) error {
	var present int
	return tx.QueryRow(ctx, `SELECT 1 FROM projects WHERE id=$1 AND org_id=$2 FOR UPDATE`, projectID, orgID).Scan(&present)
}

// checkMonitorSlot requires the project row lock from lockMonitorProject.
func (s *Server) checkMonitorSlot(ctx context.Context, tx pgx.Tx, orgID, projectID string, now time.Time) error {
	q := generated.New(tx)
	n, err := q.CountMonitorsByProject(ctx, generated.CountMonitorsByProjectParams{ProjectID: projectID, OrgID: orgID})
	if err != nil || n < maxMonitorsPerProject {
		return err
	}
	if _, err := q.PruneUnconfiguredMonitors(ctx, generated.PruneUnconfiguredMonitorsParams{
		ProjectID: projectID, OrgID: orgID,
		LastPingAt: pgtype.Timestamptz{Time: now.Add(-monitorPruneAfter), Valid: true},
	}); err != nil {
		return err
	}
	n, err = q.CountMonitorsByProject(ctx, generated.CountMonitorsByProjectParams{ProjectID: projectID, OrgID: orgID})
	if err != nil {
		return err
	}
	if n >= maxMonitorsPerProject {
		return errMonitorLimit
	}
	return nil
}

func (s *Server) recordMonitorCheckin(ctx context.Context, params generated.UpsertMonitorCheckinParams) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := s.upsertMonitorCheckinTx(ctx, tx, params); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) createMonitorWithQuota(ctx context.Context, params generated.CreateMonitorParams) (*generated.Monitor, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := s.reserveMonitorSlot(ctx, tx, params.OrgID, params.ProjectID, time.Now().UTC()); err != nil {
		return nil, err
	}
	m, err := generated.New(tx).CreateMonitor(ctx, params)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return m, nil
}
