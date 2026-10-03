package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/github"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/telemetry"
)

type githubConfigResponse struct {
	Configured bool   `json:"configured"`
	Repo       string `json:"repo"`
}

// handleGetGithubConfig reports whether GitHub is wired and for which repo. The
// token is never returned.
func (s *Server) handleGetGithubConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.q.GetGithubConfig(r.Context(), orgIDFrom(r.Context()))
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, githubConfigResponse{Configured: false})
		return
	}
	if err != nil {
		slogError(w, "get github config", err)
		return
	}
	writeJSON(w, http.StatusOK, githubConfigResponse{Configured: true, Repo: cfg.Repo})
}

func (s *Server) handleSetGithubConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo  string `json:"repo"`
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Repo = strings.TrimSpace(req.Repo)
	req.Token = strings.TrimSpace(req.Token)
	if !github.ValidRepo(req.Repo) {
		writeErr(w, http.StatusBadRequest, "repo must be in owner/name form")
		return
	}
	if req.Token == "" {
		writeErr(w, http.StatusBadRequest, "a GitHub token is required")
		return
	}
	if err := s.q.UpsertGithubConfig(r.Context(), generated.UpsertGithubConfigParams{
		OrgID: orgIDFrom(r.Context()), Repo: req.Repo, Token: s.secrets.Encrypt(req.Token),
	}); err != nil {
		slogError(w, "save github config", err)
		return
	}
	s.audit(r.Context(), "github.connect", req.Repo)
	writeJSON(w, http.StatusOK, githubConfigResponse{Configured: true, Repo: req.Repo})
}

func (s *Server) handleDeleteGithubConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.q.DeleteGithubConfig(r.Context(), orgIDFrom(r.Context())); err != nil {
		slogError(w, "delete github config", err)
		return
	}
	s.audit(r.Context(), "github.disconnect", "")
	writeJSON(w, http.StatusNoContent, nil)
}

// handleCreateGithubIssue opens a GitHub issue from a Flare issue and records
// the resulting URL on it. Idempotent-ish: returns the existing URL if already
// linked.
func (s *Server) handleCreateGithubIssue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org := orgIDFrom(ctx)
	issueID := chi.URLParam(r, "id")

	issue, err := s.store.GetIssue(ctx, issueID, org)
	if err != nil {
		var nf telemetry.ErrNotFound
		if errors.As(err, &nf) {
			writeErr(w, http.StatusNotFound, "issue not found")
			return
		}
		slogError(w, "github issue: load issue", err)
		return
	}
	if issue.GithubURL != "" {
		writeJSON(w, http.StatusOK, map[string]string{"github_url": issue.GithubURL})
		return
	}

	cfg, err := s.q.GetGithubConfig(ctx, org)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusBadRequest, "GitHub is not configured for this workspace")
		return
	}
	if err != nil {
		slogError(w, "github issue: config", err)
		return
	}

	title := issue.Title
	body := fmt.Sprintf("Reported by Flare.\n\n**Level:** %s\n**Culprit:** %s\n**Events:** %d\n\n%s/issues/%s",
		issue.Level, issue.Culprit, issue.EventCount, strings.TrimRight(s.cfg.BaseURL, "/"), issue.ID)

	token, err := s.secrets.Decrypt(cfg.Token)
	if err != nil {
		slog.Error("github: stored token cannot be decrypted; check FLARE_SECRET_KEY", "org", org, "error", err)
		writeErr(w, http.StatusBadGateway, "the stored GitHub token could not be read; reconnect the integration")
		return
	}
	connectorCtx := integration.WithScope(ctx, integration.Scope{OrgID: org, ProjectID: issue.ProjectID})
	url, err := github.CreateIssue(connectorCtx, token, cfg.Repo, title, body)
	if err != nil {
		slogError(w, "github create issue", err)
		writeErr(w, http.StatusBadGateway, "could not create the GitHub issue (check the repo and token)")
		return
	}
	if _, err := s.q.SetIssueGithubURL(ctx, generated.SetIssueGithubURLParams{ID: issue.ID, OrgID: org, GithubUrl: url}); err != nil {
		slogError(w, "github issue: save url", err)
		return
	}
	s.audit(ctx, "github.issue_create", issue.ID)
	writeJSON(w, http.StatusCreated, map[string]string{"github_url": url})
}
