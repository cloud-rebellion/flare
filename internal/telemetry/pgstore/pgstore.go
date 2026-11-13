// Package pgstore is the Postgres-backed implementation of telemetry.Store.
// It wraps the exact sqlc queries the handlers used directly before, so the
// switch to the Store interface is behaviour-preserving.
package pgstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/telemetry"
)

type PGStore struct {
	pool *pgxpool.Pool
	q    *generated.Queries
}

func New(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool, q: generated.New(pool)}
}

var _ telemetry.Store = (*PGStore)(nil)

func (s *PGStore) ListIssues(ctx context.Context, projectID, orgID string, limit, offset int32, status, q *string) ([]telemetry.Issue, error) {
	rows, err := s.q.ListIssues(ctx, generated.ListIssuesParams{
		ProjectID: projectID, OrgID: orgID, Limit: limit, Offset: offset,
		Status: nText(status), Q: nText(q),
	})
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.Issue, 0, len(rows))
	for _, r := range rows {
		out = append(out, issueFrom(r))
	}
	return out, nil
}

func (s *PGStore) CountIssues(ctx context.Context, projectID, orgID string, status, q *string) (int64, error) {
	return s.q.CountIssues(ctx, generated.CountIssuesParams{
		ProjectID: projectID, OrgID: orgID, Status: nText(status), Q: nText(q),
	})
}

func (s *PGStore) GetIssue(ctx context.Context, issueID, orgID string) (telemetry.Issue, error) {
	r, err := s.q.GetIssue(ctx, generated.GetIssueParams{ID: issueID, OrgID: orgID})
	if errors.Is(err, pgx.ErrNoRows) {
		return telemetry.Issue{}, telemetry.ErrNotFound{What: "issue"}
	}
	if err != nil {
		return telemetry.Issue{}, err
	}
	return issueFrom(r), nil
}

func (s *PGStore) ListEventsByIssue(ctx context.Context, issueID, orgID string, limit int32) ([]telemetry.Event, error) {
	rows, err := s.q.ListEventsByIssue(ctx, generated.ListEventsByIssueParams{
		IssueID: pgText(issueID), OrgID: orgID, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.Event, 0, len(rows))
	for _, e := range rows {
		out = append(out, telemetry.Event{
			ID: e.ID, Level: e.Level, Message: e.Message,
			ExceptionType: e.ExceptionType, ExceptionValue: e.ExceptionValue,
			Platform: e.Platform, Environment: e.Environment, Release: e.Release,
			Stacktrace: e.Stacktrace, TraceID: e.TraceID.String, SpanID: e.SpanID.String,
			Payload: e.Payload, ReceivedAt: e.ReceivedAt.Time,
		})
	}
	return out, nil
}

func (s *PGStore) SearchLogs(ctx context.Context, projectID, orgID string, f telemetry.LogFilter) ([]telemetry.Log, error) {
	rows, err := s.q.SearchLogs(ctx, generated.SearchLogsParams{
		ProjectID: projectID, OrgID: orgID, Limit: f.Limit,
		Severity: nText(f.Severity), Q: nText(f.Query), TraceID: nText(f.TraceID),
		Since: nTs(f.Since), Before: nTs(f.Before),
	})
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.Log, 0, len(rows))
	for _, l := range rows {
		out = append(out, telemetry.Log{
			ID: l.ID, Severity: l.Severity, Body: l.Body, Attributes: l.Attributes,
			TraceID: l.TraceID, SpanID: l.SpanID, ObservedAt: l.ObservedAt.Time,
		})
	}
	return out, nil
}

func (s *PGStore) ListTraces(ctx context.Context, projectID, orgID string, since time.Time, limit int32) ([]telemetry.TraceSummary, error) {
	rows, err := s.q.ListTraces(ctx, generated.ListTracesParams{
		ProjectID: projectID, OrgID: orgID,
		Since: pgtype.Timestamptz{Time: since, Valid: true}, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.TraceSummary, 0, len(rows))
	for _, t := range rows {
		dur := 0.0
		if t.Started.Valid && t.Ended.Valid {
			dur = float64(t.Ended.Time.Sub(t.Started.Time).Microseconds()) / 1000.0
		}
		out = append(out, telemetry.TraceSummary{
			TraceID: t.TraceID, RootName: t.RootName, SpanCount: t.SpanCount,
			HasError: t.HasError, DurationMs: dur, Started: t.Started.Time,
		})
	}
	return out, nil
}

func (s *PGStore) GetTraceSpans(ctx context.Context, traceID, projectID, orgID string, limit int32) ([]telemetry.Span, error) {
	rows, err := s.q.GetTraceSpans(ctx, generated.GetTraceSpansParams{
		TraceID: traceID, ProjectID: projectID, OrgID: orgID, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.Span, 0, len(rows))
	for _, sp := range rows {
		out = append(out, telemetry.Span{
			SpanID: sp.SpanID, ParentSpanID: sp.ParentSpanID, Name: sp.Name,
			Kind: sp.Kind, Status: sp.Status,
			StartUnixMs: sp.StartTime.Time.UnixMilli(), DurationMs: sp.DurationMs, Attributes: sp.Attributes,
		})
	}
	return out, nil
}

func (s *PGStore) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.pool.Ping(ctx) == nil
}

func issueFrom(i *generated.Issue) telemetry.Issue {
	return telemetry.Issue{
		ID: i.ID, ProjectID: i.ProjectID, OrgID: i.OrgID, Fingerprint: i.Fingerprint,
		Title: i.Title, Culprit: i.Culprit, Level: i.Level, Status: i.Status, Platform: i.Platform,
		FirstSeen: i.FirstSeen.Time, LastSeen: i.LastSeen.Time, EventCount: i.EventCount,
		GithubURL: i.GithubUrl, FirstRelease: i.FirstRelease, AITriage: i.AiTriage,
		Sensitive: i.Sensitive,
	}
}

func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

func nText(p *string) pgtype.Text {
	if p == nil {
		return pgtype.Text{}
	}
	return pgText(*p)
}

func nTs(p *time.Time) pgtype.Timestamptz {
	if p == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *p, Valid: true}
}

func (s *PGStore) ListMetricNames(ctx context.Context, projectID, orgID string, limit int32) ([]telemetry.MetricName, error) {
	rows, err := s.q.ListMetricNames(ctx, generated.ListMetricNamesParams{ProjectID: projectID, OrgID: orgID, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.MetricName, 0, len(rows))
	for _, r := range rows {
		out = append(out, telemetry.MetricName{
			Name: r.Name, Kind: r.Kind, Points: r.Points, LastSeen: r.LastSeen.Time,
		})
	}
	return out, nil
}

func (s *PGStore) QueryMetricSeries(ctx context.Context, projectID, orgID, name string, since time.Time, limit int32) ([]telemetry.MetricPoint, error) {
	rows, err := s.q.QueryMetricSeries(ctx, generated.QueryMetricSeriesParams{
		ProjectID: projectID, OrgID: orgID, Name: name,
		ObservedAt: pgtype.Timestamptz{Time: since, Valid: true}, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.MetricPoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, telemetry.MetricPoint{
			Value: r.Value, Labels: r.Labels, ObservedAt: r.ObservedAt.Time,
		})
	}
	return out, nil
}
