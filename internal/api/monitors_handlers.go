package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

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

	params := generated.UpsertMonitorCheckinParams{
		ID:         id.New(),
		OrgID:      project.OrgID,
		ProjectID:  project.ID,
		Slug:       slug,
		LastPingAt: pgtype.Timestamptz{Time: now, Valid: true},
		LastStatus: checkStatus,
		State:      state,
	}
	if state == "failed" {
		// Resolve only channels routed to this project. A transient routing
		// load error cannot acknowledge a failed check-in whose alert never
		// entered the durable queue.
		channels, err := s.q.ListEnabledChannelsForProject(r.Context(), generated.ListEnabledChannelsForProjectParams{
			OrgID: project.OrgID, ProjectID: project.ID,
		})
		if err != nil {
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusServiceUnavailable, "monitor acceptance is temporarily unavailable")
			return
		}
		if len(channels) > 0 {
			if err := s.recordFailedMonitorCheckin(r.Context(), params, channels); err != nil {
				writeMonitorCheckinError(w, project.ID, slug, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
	}
	if err := s.recordMonitorCheckin(r.Context(), params); err != nil {
		writeMonitorCheckinError(w, project.ID, slug, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeMonitorCheckinError(w http.ResponseWriter, projectID, slug string, err error) {
	if errors.Is(err, errMonitorLimit) {
		slog.Warn("monitor check-in refused: project is at its monitor cap",
			"project_id", projectID, "slug", slug, "cap", maxMonitorsPerProject)
		writeErr(w, http.StatusTooManyRequests, "this project has reached its monitor limit")
		return
	}
	slog.Error("monitor check-in acceptance failed", "project_id", projectID, "slug", slug, "error", err)
	w.Header().Set("Retry-After", "1")
	writeErr(w, http.StatusServiceUnavailable, "monitor acceptance is temporarily unavailable")
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
	m, err := s.createMonitorWithQuota(r.Context(), generated.CreateMonitorParams{
		ID:              id.New(),
		OrgID:           proj.OrgID,
		ProjectID:       proj.ID,
		Slug:            req.Slug,
		Name:            req.Name,
		IntervalSeconds: req.IntervalSeconds,
		GraceSeconds:    req.GraceSeconds,
	})
	if err != nil {
		if errors.Is(err, errMonitorLimit) {
			writeErr(w, http.StatusTooManyRequests, "this project has reached its monitor limit")
			return
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeErr(w, http.StatusBadRequest, "could not create monitor (slug may already exist)")
			return
		}
		slogError(w, "create monitor", err)
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
