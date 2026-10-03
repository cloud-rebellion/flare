package api

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/alerts"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// monitorSlugRe bounds a check-in slug to a safe, URL-friendly token.
var monitorSlugRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

type monitorResponse struct {
	ID              string  `json:"id"`
	Slug            string  `json:"slug"`
	Name            string  `json:"name"`
	IntervalSeconds int32   `json:"interval_seconds"`
	GraceSeconds    int32   `json:"grace_seconds"`
	LastPingAt      *string `json:"last_ping_at"`
	LastStatus      string  `json:"last_status"`
	State           string  `json:"state"`
	CheckinURL      string  `json:"checkin_url"`
}

const (
	// maxMonitorsPerProject bounds auto-created monitors. Far above any real
	// cron fleet and far below the 1.7M rows a day a DSN key could otherwise
	// mint.
	maxMonitorsPerProject = 500
	// monitorPruneAfter is how long an unconfigured monitor may stay quiet
	// before the cap sweep removes it. A CONFIGURED monitor is never pruned:
	// going quiet is exactly what it exists to report.
	monitorPruneAfter = 30 * 24 * time.Hour
)

// checkinURL is the DSN-authed URL a scheduled job pings each run. The public
// key is the ingest key (already embedded in the project DSN), so carrying it as
// a query param lets a cron check in with a bare curl.
func (s *Server) checkinURL(proj *generated.Project, slug string) string {
	return strings.TrimRight(s.cfg.BaseURL, "/") + "/api/" + proj.DsnID + "/checkins/" + slug + "?sentry_key=" + proj.PublicKey
}

// toMonitorResponse renders a monitor. checkin_url embeds the project's ingest
// public key, so it is the same write credential as the DSN and is omitted for
// a viewer for the same reason: GET /api/{dsnID}/checkins/{slug} CREATES the
// monitor, so a read-only credential could persist rows.
func (s *Server) toMonitorResponse(ctx context.Context, m *generated.Monitor, proj *generated.Project) monitorResponse {
	out := monitorResponse{
		ID:              m.ID,
		Slug:            m.Slug,
		Name:            m.Name,
		IntervalSeconds: m.IntervalSeconds,
		GraceSeconds:    m.GraceSeconds,
		LastPingAt:      tsPtr(m.LastPingAt),
		LastStatus:      m.LastStatus,
		State:           m.State,
	}
	if roleAtLeast(roleFrom(ctx), "member") {
		out.CheckinURL = s.checkinURL(proj, m.Slug)
	}
	return out
}

// handleCheckin records a scheduled-job check-in. DSN-authed (same as ingest),
// so a cron pings it with the project key. ?status=error marks the run failed.
func (s *Server) handleCheckin(w http.ResponseWriter, r *http.Request) {
	project, ok := s.authIngest(w, r)
	if !ok {
		return
	}
	slug := chi.URLParam(r, "slug")
	if !monitorSlugRe.MatchString(slug) {
		writeErr(w, http.StatusBadRequest, "invalid monitor slug")
		return
	}
	checkStatus, state := "ok", "ok"
	switch strings.ToLower(r.URL.Query().Get("status")) {
	case "error", "fail", "failed":
		checkStatus, state = "error", "failed"
	}
	now := time.Now().UTC()

	// Prior state drives transition-only failure alerting (see below).
	priorState := ""
	if prior, err := s.q.GetMonitorBySlug(r.Context(), generated.GetMonitorBySlugParams{
		ProjectID: project.ID, OrgID: project.OrgID, Slug: slug,
	}); err == nil {
		priorState = prior.State
	}

	// Bound the monitors one project can accumulate.
	//
	// UpsertMonitorCheckin CREATES the monitor on first ping and the slug is
	// caller-chosen on a DSN-authed endpoint, so the slug regex bounded LENGTH
	// and nothing bounded CARDINALITY: at 1200 requests/min per key that is 1.7M
	// monitor rows a day per project. ListDueMonitors then sequentially scans
	// that table every five minutes forever (EXTRACT(EPOCH ...) per row, so no
	// index applies) and ListMonitorsByProject returns all of them to the
	// dashboard.
	//
	// The cap only applies to a slug that does not exist yet, so an established
	// monitor never stops being able to check in. Before refusing, sweep the
	// unconfigured ones that have gone quiet: an honest project that churned
	// through job names self-heals instead of wedging.
	if priorState == "" {
		n, cerr := s.q.CountMonitorsByProject(r.Context(), generated.CountMonitorsByProjectParams{
			ProjectID: project.ID, OrgID: project.OrgID,
		})
		if cerr == nil && n >= maxMonitorsPerProject {
			if _, perr := s.q.PruneUnconfiguredMonitors(r.Context(), generated.PruneUnconfiguredMonitorsParams{
				ProjectID: project.ID, OrgID: project.OrgID,
				LastPingAt: pgtype.Timestamptz{Time: now.Add(-monitorPruneAfter), Valid: true},
			}); perr != nil {
				slog.Warn("prune unconfigured monitors failed", "project_id", project.ID, "error", perr)
			}
			n, cerr = s.q.CountMonitorsByProject(r.Context(), generated.CountMonitorsByProjectParams{
				ProjectID: project.ID, OrgID: project.OrgID,
			})
			if cerr == nil && n >= maxMonitorsPerProject {
				slog.Warn("monitor check-in refused: project is at its monitor cap",
					"project_id", project.ID, "slug", slug, "cap", maxMonitorsPerProject)
				writeErr(w, http.StatusTooManyRequests, "this project has reached its monitor limit")
				return
			}
		}
	}

	if _, err := s.q.UpsertMonitorCheckin(r.Context(), generated.UpsertMonitorCheckinParams{
		ID:         id.New(),
		OrgID:      project.OrgID,
		ProjectID:  project.ID,
		Slug:       slug,
		LastPingAt: pgtype.Timestamptz{Time: now, Valid: true},
		LastStatus: checkStatus,
		State:      state,
	}); err != nil {
		slogError(w, "monitor checkin", err)
		return
	}

	// A run that reports failure pages immediately, but only on the ok->failed
	// transition so a persistently-failing job does not spam an alert per run.
	// Rate-cap per (org, monitor). The transition test alone is not a cap: the
	// check-in endpoint is DSN-authed and the caller chooses ?status=, so
	// alternating ok/error makes every second request a fresh ok->failed
	// transition, and at the ingest budget that is hundreds of emails a minute
	// to the org's channels.
	// Two caps, and both are needed. The per-monitor one stops a single honestly
	// flapping cron from alerting on every run; the per-ORG one bounds the total
	// when the slug itself is attacker-chosen, which it is: this endpoint is
	// DSN-authed and UpsertMonitorCheckin creates the monitor, so an unlimited
	// number of slugs is reachable with a public key. Checking the per-monitor
	// limiter FIRST keeps the org budget from being consumed by a monitor that
	// was going to be suppressed anyway.
	if state == "failed" && priorState != "failed" &&
		s.testLimiter.Allow("monitor-failed:"+project.OrgID+":"+slug) &&
		s.monitorAlertLimiter.Allow("monitor-failed-org:"+project.OrgID) {
		pid, name, org := project.ID, project.Name, project.OrgID
		// Bounded like the other ingest-side background work: this runs on the
		// DSN-authed check-in endpoint, so anyone holding a project's public key
		// could otherwise loop ok->failed transitions and spawn one unbounded
		// detached goroutine (plus one outbound alert) per request.
		s.goBackgroundFor(org, "monitor-failed", 15*time.Second, func(ctx context.Context) {
			// A monitor belongs to a project, so this respects routing.
			s.dispatchToProject(ctx, org, pid, alerts.Notification{
				ProjectID:   pid,
				ProjectName: name,
				Title:       "Monitor failed: " + slug,
				Level:       "warning",
				Reason:      "Check-in reported failure",
				URL:         strings.TrimRight(s.cfg.BaseURL, "/") + "/projects/" + pid,
			})
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListMonitors(w http.ResponseWriter, r *http.Request) {
	proj, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{
		ID: chi.URLParam(r, "id"), OrgScope: orgIDFrom(r.Context()),
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	mons, err := s.q.ListMonitorsByProject(r.Context(), generated.ListMonitorsByProjectParams{
		ProjectID: proj.ID, OrgID: proj.OrgID,
	})
	if err != nil {
		slogError(w, "list monitors", err)
		return
	}
	out := make([]monitorResponse, 0, len(mons))
	for _, m := range mons {
		out = append(out, s.toMonitorResponse(r.Context(), m, proj))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateMonitor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Slug            string `json:"slug"`
		Name            string `json:"name"`
		IntervalSeconds int32  `json:"interval_seconds"`
		GraceSeconds    int32  `json:"grace_seconds"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Slug = strings.TrimSpace(req.Slug)
	if !monitorSlugRe.MatchString(req.Slug) {
		writeErr(w, http.StatusBadRequest, "slug must be 1-64 chars: letters, digits, . _ -")
		return
	}
	if req.IntervalSeconds < 0 || req.GraceSeconds < 0 {
		writeErr(w, http.StatusBadRequest, "interval and grace must be >= 0")
		return
	}
	if req.GraceSeconds == 0 {
		req.GraceSeconds = 300
	}
	proj, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{
		ID: chi.URLParam(r, "id"), OrgScope: orgIDFrom(r.Context()),
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	m, err := s.q.CreateMonitor(r.Context(), generated.CreateMonitorParams{
		ID:              id.New(),
		OrgID:           proj.OrgID,
		ProjectID:       proj.ID,
		Slug:            req.Slug,
		Name:            req.Name,
		IntervalSeconds: req.IntervalSeconds,
		GraceSeconds:    req.GraceSeconds,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not create monitor (slug may already exist)")
		return
	}
	s.audit(r.Context(), "monitor.create", req.Slug)
	writeJSON(w, http.StatusCreated, s.toMonitorResponse(r.Context(), m, proj))
}

func (s *Server) handleUpdateMonitor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string `json:"name"`
		IntervalSeconds int32  `json:"interval_seconds"`
		GraceSeconds    int32  `json:"grace_seconds"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.IntervalSeconds < 0 || req.GraceSeconds < 0 {
		writeErr(w, http.StatusBadRequest, "interval and grace must be >= 0")
		return
	}
	if req.GraceSeconds == 0 {
		req.GraceSeconds = 300
	}
	org := orgIDFrom(r.Context())
	m, err := s.q.UpdateMonitorConfig(r.Context(), generated.UpdateMonitorConfigParams{
		ID:              chi.URLParam(r, "id"),
		OrgID:           org,
		Name:            req.Name,
		IntervalSeconds: req.IntervalSeconds,
		GraceSeconds:    req.GraceSeconds,
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "monitor not found")
		return
	}
	proj, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{ID: m.ProjectID, OrgScope: org})
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	s.audit(r.Context(), "monitor.update", m.Slug)
	writeJSON(w, http.StatusOK, s.toMonitorResponse(r.Context(), m, proj))
}

func (s *Server) handleDeleteMonitor(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.DeleteMonitor(r.Context(), generated.DeleteMonitorParams{
		ID: chi.URLParam(r, "id"), OrgID: orgIDFrom(r.Context()),
	})
	if err != nil {
		slogError(w, "delete monitor", err)
		return
	}
	if rows == 0 {
		writeErr(w, http.StatusNotFound, "monitor not found")
		return
	}
	s.audit(r.Context(), "monitor.delete", chi.URLParam(r, "id"))
	writeJSON(w, http.StatusNoContent, nil)
}
