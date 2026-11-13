package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// audit appends a sensitive action to the org's audit trail. Best-effort: a
// failure is logged, never propagated to the caller. Actor is the session user
// (NULL for API-key actions).
func (s *Server) audit(ctx context.Context, action, target string) {
	actor := pgtype.Text{}
	if uid := userIDFrom(ctx); uid != "" {
		actor = pgText(uid)
	}
	if err := s.q.InsertAuditLog(ctx, generated.InsertAuditLogParams{
		ID: id.New(), OrgID: orgIDFrom(ctx), ActorUserID: actor, Action: action, Target: target,
	}); err != nil {
		slog.Warn("audit log insert failed", "action", action, "error", err)
	}
}

type auditEntry struct {
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) handleListAuditLog(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListAuditLog(r.Context(), orgIDFrom(r.Context()))
	if err != nil {
		slogError(w, "list audit log", err)
		return
	}
	out := make([]auditEntry, 0, len(rows))
	for _, row := range rows {
		actor := "API key"
		if row.ActorEmail.Valid {
			actor = row.ActorEmail.String
		}
		out = append(out, auditEntry{Action: row.Action, Target: row.Target, Actor: actor, CreatedAt: row.CreatedAt.Time})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleExport returns the org's structured data as a downloadable JSON bundle
// (data portability). Raw events/logs/spans are excluded by size; issue
// metadata is included.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org := orgIDFrom(ctx)

	orgRow, err := s.q.GetOrgByID(ctx, org)
	if err != nil {
		slogError(w, "export: org", err)
		return
	}
	users, _ := s.q.ListUsersByOrg(ctx, org)
	channels, _ := s.q.ListNotificationChannelsByOrg(ctx, org)
	projects, _ := s.q.ListProjectsByOrg(ctx, org)

	members := make([]map[string]any, 0, len(users))
	for _, u := range users {
		members = append(members, map[string]any{"email": u.Email, "role": u.Role, "created_at": u.CreatedAt.Time})
	}
	chans := make([]map[string]any, 0, len(channels))
	for _, c := range channels {
		// Redact like every other read path. The export was returning the RAW
		// stored config, so a Slack or webhook URL (a bearer secret: whoever
		// holds it can post into the org's channel) was handed out in full to
		// anyone who could hit the export endpoint.
		chans = append(chans, map[string]any{
			"type": c.Type, "config": s.redactChannelConfig(c.Type, c.Config), "enabled": c.Enabled,
		})
	}

	// Data portability means ALL of it. The export used to take the first 1000
	// issues per project and silently drop the rest, and swallow any query
	// error, so a workspace with more than that got a quietly incomplete bundle
	// with nothing to say so. Page through instead, and record what happened.
	const exportPage = 1000
	var exportWarnings []string

	projOut := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		// ListAllIssuesForExport, not ListIssues: the latter carries
		// `level NOT IN ('info','debug')` for the dashboard, so the bundle
		// whose own comment says "data portability means ALL of it" was
		// dropping every informational issue silently.
		//
		// Keyset paging, not OFFSET. last_seen MUTATES on every ingested event,
		// so an OFFSET walk over a DESC last_seen ordering is computed against
		// a list that moved between pages: rows are skipped and repeated under
		// live ingest, which is the exact bug SearchLogs was switched away from
		// OFFSET to avoid.
		var issues []*generated.Issue
		var cursorSeen pgtype.Timestamptz
		var cursorID pgtype.Text
		for {
			page, err := s.q.ListAllIssuesForExport(ctx, generated.ListAllIssuesForExportParams{
				ProjectID: p.ID, OrgID: org, Limit: exportPage,
				AfterLastSeen: cursorSeen, AfterID: cursorID,
			})
			if err != nil {
				exportWarnings = append(exportWarnings,
					"issues for project "+p.Slug+" are incomplete: "+err.Error())
				break
			}
			issues = append(issues, page...)
			if len(page) < int(exportPage) {
				break
			}
			last := page[len(page)-1]
			cursorSeen, cursorID = last.LastSeen, pgText(last.ID)
		}
		issueOut := make([]map[string]any, 0, len(issues))
		for _, i := range issues {
			issueOut = append(issueOut, map[string]any{
				"title": i.Title, "level": i.Level, "status": i.Status, "culprit": i.Culprit,
				"event_count": i.EventCount, "first_seen": i.FirstSeen.Time, "last_seen": i.LastSeen.Time,
				"first_release": i.FirstRelease,
			})
		}
		rules, _ := s.q.ListAlertRulesByProject(ctx, generated.ListAlertRulesByProjectParams{ProjectID: p.ID, OrgID: org})
		ruleOut := make([]map[string]any, 0, len(rules))
		for _, ru := range rules {
			ruleOut = append(ruleOut, map[string]any{"name": ru.Name, "type": ru.Type, "threshold": ru.Threshold, "window_minutes": ru.WindowMinutes})
		}
		releases, _ := s.q.ListReleasesByProject(ctx, generated.ListReleasesByProjectParams{ProjectID: p.ID, OrgID: org})
		relOut := make([]map[string]any, 0, len(releases))
		for _, rl := range releases {
			relOut = append(relOut, map[string]any{"version": rl.Version, "created_at": rl.CreatedAt.Time, "new_issues": rl.NewIssues})
		}
		projOut = append(projOut, map[string]any{
			"name": p.Name, "slug": p.Slug, "platform": p.Platform, "dsn": s.dsn(p.PublicKey, p.DsnID),
			"issues": issueOut, "alert_rules": ruleOut, "releases": relOut,
		})
	}

	s.audit(ctx, "data.export", orgRow.Name)
	w.Header().Set("Content-Disposition", "attachment; filename=flare-export.json")
	out := map[string]any{
		"exported_at": time.Now(),
		"org":         map[string]any{"name": orgRow.Name, "slug": orgRow.Slug, "created_at": orgRow.CreatedAt.Time},
		"members":     members,
		"channels":    chans,
		"projects":    projOut,
		// Stated explicitly so a recipient knows what this bundle is and is not.
		"note": "Structured data only. Raw events, logs, spans and metrics are excluded by size; notification-channel secrets are redacted.",
	}
	if len(exportWarnings) > 0 {
		out["incomplete"] = true
		out["warnings"] = exportWarnings
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteOrg permanently erases the whole workspace (right to erasure).
// Owner-only; cascades all org data and destroys the caller's session.
func (s *Server) handleDeleteOrg(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org := orgIDFrom(ctx)

	// Workspace erasure is the single most destructive action in the product
	// and it was the one with no record at all. An audit_log row is no use
	// here: audit_log is FK-linked to orgs, so the cascade below deletes the
	// evidence along with everything else. The durable record is the server
	// log, written BEFORE the transaction so it exists whether or not the
	// delete succeeds.
	slog.Warn("workspace erasure requested", "org_id", org, "actor_user_id", userIDFrom(ctx))

	// Right-to-erasure must reach the raw telemetry. events/logs/spans are
	// partitioned hot tables with NO foreign key to projects/orgs, so the
	// ON DELETE CASCADE from orgs never touches them (this is exactly why
	// handleDeleteProject deletes them by hand). Delete every project's
	// telemetry first, then drop the org so its FK-linked rows (projects,
	// issues, source maps, releases, channels, keys, audit log) cascade.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		slogError(w, "delete org: begin tx", err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)

	// The operator workspace owns instance-wide settings (data retention) and
	// receives Flare's own security events. Deleting it while tenants remain
	// would leave the instance with no operator, or, before migration 093,
	// hand those controls to whichever tenant was next-oldest. Refuse; it can
	// go once it is the last workspace on the instance. The lock is shared with
	// createOrgWithOwner, so no registration can commit between this check and
	// the delete.
	if err := qtx.LockOperatorOrg(ctx); err != nil {
		slogError(w, "delete org: lock", err)
		return
	}
	self, err := qtx.GetOrgByID(ctx, org)
	if err != nil {
		slogError(w, "delete org: read org", err)
		return
	}
	if self.IsOperator {
		n, err := qtx.CountOrgs(ctx)
		if err != nil {
			slogError(w, "delete org: count orgs", err)
			return
		}
		if n > 1 {
			slog.Warn("workspace erasure refused: operator workspace with other workspaces on the instance", "org_id", org, "orgs", n)
			writeErr(w, http.StatusConflict, "this is the operator workspace of the instance: it manages instance-wide settings such as data retention, so it cannot be deleted while other workspaces exist")
			return
		}
	}

	// Collect the member ids BEFORE the cascade removes the user rows: the
	// sessions table has no org column and no foreign key, so it is the one
	// store an org delete leaves fully intact unless it is cleared explicitly.
	members, err := qtx.ListUsersByOrg(ctx, org)
	if err != nil {
		slogError(w, "delete org: list members", err)
		return
	}
	userIDs := make([]string, 0, len(members))
	for _, m := range members {
		userIDs = append(userIDs, m.ID)
	}

	projects, err := qtx.ListProjectsByOrg(ctx, org)
	if err != nil {
		slogError(w, "delete org: list projects", err)
		return
	}
	for _, p := range projects {
		if err := qtx.DeleteProjectEvents(ctx, generated.DeleteProjectEventsParams{ProjectID: p.ID, OrgID: org}); err != nil {
			slogError(w, "delete org events", err)
			return
		}
		if err := qtx.DeleteProjectLogs(ctx, generated.DeleteProjectLogsParams{ProjectID: p.ID, OrgID: org}); err != nil {
			slogError(w, "delete org logs", err)
			return
		}
		if err := qtx.DeleteProjectSpans(ctx, generated.DeleteProjectSpansParams{ProjectID: p.ID, OrgID: org}); err != nil {
			slogError(w, "delete org spans", err)
			return
		}
		if err := qtx.DeleteProjectMetrics(ctx, generated.DeleteProjectMetricsParams{ProjectID: p.ID, OrgID: org}); err != nil {
			slogError(w, "delete org metrics", err)
			return
		}
	}
	if len(userIDs) > 0 {
		if err := qtx.DeleteSessionsForUsers(ctx, userIDs); err != nil {
			slogError(w, "delete org sessions", err)
			return
		}
	}
	if err := qtx.DeleteOrg(ctx, org); err != nil {
		slogError(w, "delete org", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slogError(w, "delete org: commit", err)
		return
	}

	// Erase the org's telemetry from the Parquet cold tier too (aged-out data
	// the hot-tier delete cannot reach). This runs after the committed delete,
	// so it cannot be rolled back, but its OUTCOME must reach the caller: this
	// is a right-to-erasure request, and answering a bare 204 "done" while the
	// S3 cold tier still holds the org's telemetry (PurgeColdScope explicitly
	// does not support S3) is a compliance claim Flare cannot back.
	s.invalidateSecCaches(org)

	coldErr := ""
	if s.analytics != nil {
		if err := s.analytics.PurgeColdScope(ctx, "org_id", org); err != nil {
			slog.Error("cold-tier purge failed on org delete", "org", org, "err", err)
			coldErr = err.Error()
		}
	}

	if coldErr != "" {
		// Record BEFORE destroying the session, while the actor is still known.
		s.recordPartialErasure(ctx, org, "", "org_id", org, userIDFrom(ctx), coldErr)
	}
	_ = s.sessions.Destroy(ctx)
	if coldErr != "" {
		// 202, not 200. A client that only checks the status code reads 200 as
		// "erased" and moves on, which is the wrong thing to tell someone who
		// exercised a right to erasure. 202 says accepted-but-incomplete, and the
		// body says exactly what is outstanding.
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":         "partial",
			"hot_tier":       "erased",
			"cold_tier":      "NOT erased",
			"cold_tier_note": coldErr,
		})
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// recordPartialErasure persists an erasure obligation that was not fulfilled.
//
// It is written BEFORE the response so a crash between the two cannot lose it,
// and into a table with no foreign keys so it survives the deletion of the org
// and project it names. That is the whole reason it exists: for an org deletion
// there is no tenant left to notice, no audit row that outlives the org, and
// nobody to ask. Without this the obligation lived only in a log line.
func (s *Server) recordPartialErasure(ctx context.Context, org, project, column, value, by, note string) {
	// Detach from the request context before the durability write. Both callers
	// pass r.Context(), which a client disconnect or proxy timeout can cancel at
	// any moment, and here that moment is AFTER the hot-tier delete has already
	// committed. A cancel between the commit and this insert would drop the only
	// durable trace of an unfulfilled erasure - the exact pre-027 state this
	// table exists to end, and unrecoverable for an org delete whose org row is
	// already gone. Same pattern the delivery record uses (server.go), applied
	// here because the obligation is the more critical of the two to persist.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.q.RecordPendingErasure(ctx, generated.RecordPendingErasureParams{
		ID:          id.New(),
		OrgID:       org,
		ProjectID:   pgtype.Text{String: project, Valid: project != ""},
		ScopeColumn: column,
		ScopeValue:  value,
		RequestedBy: pgtype.Text{String: by, Valid: by != ""},
		ColdTier:    "NOT erased",
		Reason:      note,
	}); err != nil {
		// Log loudly: at this point the data is still out there and the only
		// other record is this line.
		slog.Error("FAILED to record an unfulfilled erasure obligation",
			"org", org, "project", project, "scope", column+"="+value, "note", note, "err", err)
	}
}

// LogOpenErasures reports every unfulfilled erasure obligation. Called at
// startup so the obligation keeps announcing itself until an operator clears the
// row: the tenant who asked for it may no longer exist.
func (s *Server) LogOpenErasures(ctx context.Context) {
	rows, err := s.q.ListOpenErasures(ctx)
	if err != nil {
		slog.Warn("could not read pending erasures", "err", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	slog.Error("UNFULFILLED right-to-erasure obligations outstanding",
		"count", len(rows),
		"action", "erase the listed scopes from the object store, then set completed_at on the row")
	for _, r := range rows {
		slog.Error("  pending erasure",
			"requested_at", r.RequestedAt.Time.Format(time.RFC3339),
			"org", r.OrgID,
			"scope", r.ScopeColumn+"="+r.ScopeValue,
			"reason", r.Reason)
	}
}

// RunErasureReminder re-surfaces outstanding erasure obligations on a periodic
// ticker, not only at boot. LogOpenErasures alone fires once at startup, so a
// server that stays up for weeks after an org deletion stops announcing the
// obligation and the operator loses the reminder to the log rotation. Repeating
// it keeps a long-running instance honest about what it still owes. Best-effort:
// a read error is logged and the loop continues.
func (s *Server) RunErasureReminder(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.LogOpenErasures(ctx)
		}
	}
}

type pendingErasure struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id,omitempty"`
	ScopeColumn string    `json:"scope_column"`
	ScopeValue  string    `json:"scope_value"`
	ColdTier    string    `json:"cold_tier"`
	Reason      string    `json:"reason"`
	RequestedAt time.Time `json:"requested_at"`
}

// handleListOpenErasures returns this org's outstanding erasure obligations so an
// admin can see what still has to be erased from the object store and get the id
// to mark it done. Admin-gated at the router. Scoped to the caller's org: it
// cannot surface a DELETED org's own obligation (there is no org to be admin of),
// which is why the operator log path in LogOpenErasures spans every org.
func (s *Server) handleListOpenErasures(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListOpenErasuresByOrg(r.Context(), orgIDFrom(r.Context()))
	if err != nil {
		slogError(w, "list open erasures", err)
		return
	}
	out := make([]pendingErasure, 0, len(rows))
	for _, row := range rows {
		out = append(out, pendingErasure{
			ID:          row.ID,
			ProjectID:   row.ProjectID.String,
			ScopeColumn: row.ScopeColumn,
			ScopeValue:  row.ScopeValue,
			ColdTier:    row.ColdTier,
			Reason:      row.Reason,
			RequestedAt: row.RequestedAt.Time,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCompleteErasure marks one obligation fulfilled once the operator has
// erased the named scope from the object store by hand. Admin-gated. The write is
// scoped to (id, org) so an admin can never clear another tenant's obligation by
// guessing its id, and a wrong id or already-completed row returns 404 rather than
// a phantom success. This is the app path that ends the old "hand-write SQL to
// clear the row" requirement for a still-living org's obligations.
func (s *Server) handleCompleteErasure(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := pgtype.Text{}
	if uid := userIDFrom(ctx); uid != "" {
		actor = pgText(uid)
	}
	rows, err := s.q.CompletePendingErasure(ctx, generated.CompletePendingErasureParams{
		ID:          chi.URLParam(r, "id"),
		OrgID:       orgIDFrom(ctx),
		CompletedBy: actor,
	})
	if err != nil {
		slogError(w, "complete erasure", err)
		return
	}
	if rows == 0 {
		writeErr(w, http.StatusNotFound, "no open erasure obligation with that id in this org")
		return
	}
	s.audit(ctx, "erasure.complete", chi.URLParam(r, "id"))
	writeJSON(w, http.StatusNoContent, nil)
}
