package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/bright-interaction/flare/internal/automationoutbox"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

func (s *Server) handleListAutomationDeliveryReceipts(w http.ResponseWriter, r *http.Request) {
	org, project := orgIDFrom(r.Context()), chi.URLParam(r, "id")
	if org == "" {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	_, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{ID: project, OrgScope: org})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		slogError(w, "list automation delivery project", err)
		return
	}
	limit := 25
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			writeErr(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
	}
	page, err := automationoutbox.New(s.pool).ListDeliveryReceipts(r.Context(), org, project, r.URL.Query().Get("issue_id"), limit, r.URL.Query().Get("cursor"))
	if errors.Is(err, automationoutbox.ErrDeliveryPage) {
		writeErr(w, http.StatusBadRequest, "invalid automation delivery pagination or issue filter")
		return
	}
	if err != nil {
		slogError(w, "list automation deliveries", err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleGetAutomationDeliveryReceipt reports one original notice's delivery
// outcome. A receiver run ID proves enqueue acknowledgement only, never AI or
// workflow success, human approval, a completed fix or verified deployment.
func (s *Server) handleGetAutomationDeliveryReceipt(w http.ResponseWriter, r *http.Request) {
	org, project := orgIDFrom(r.Context()), chi.URLParam(r, "id")
	if org == "" {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if noticeID := chi.URLParam(r, "noticeID"); len(noticeID) == 0 || len(noticeID) > 128 {
		writeErr(w, http.StatusNotFound, "automation delivery not found")
		return
	}
	_, err := s.q.GetProjectByID(r.Context(), generated.GetProjectByIDParams{ID: project, OrgScope: org})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		slogError(w, "get automation delivery project", err)
		return
	}
	receipt, err := automationoutbox.New(s.pool).GetDeliveryReceipt(r.Context(), org, project, chi.URLParam(r, "noticeID"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "automation delivery not found")
		return
	}
	if err != nil {
		slogError(w, "get automation delivery", err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}
