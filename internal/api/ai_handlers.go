package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/bright-interaction/flare/internal/ai"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/netguard"
	"github.com/bright-interaction/flare/internal/telemetry"
)

type aiConfigResponse struct {
	Enabled           bool   `json:"enabled"`
	BaseURL           string `json:"base_url"`
	Model             string `json:"model"`
	Format            string `json:"format"`
	AutoTriage        bool   `json:"auto_triage"`
	TriageDailyBudget int32  `json:"triage_daily_budget"`
}

// defaultTriageDailyBudget bounds auto-triage BYOAI spend for an org that never
// set an explicit budget.
const defaultTriageDailyBudget = 50

func (s *Server) handleGetAIConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.q.GetAIConfig(r.Context(), orgIDFrom(r.Context()))
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, aiConfigResponse{Format: "openai", TriageDailyBudget: defaultTriageDailyBudget})
		return
	}
	if err != nil {
		slogError(w, "get ai config", err)
		return
	}
	writeJSON(w, http.StatusOK, aiConfigResponse{
		Enabled: cfg.Enabled, BaseURL: cfg.BaseUrl, Model: cfg.Model, Format: cfg.Format,
		AutoTriage: cfg.AutoTriage, TriageDailyBudget: cfg.TriageDailyBudget,
	})
}

func (s *Server) handleSetAIConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL           string `json:"base_url"`
		APIKey            string `json:"api_key"`
		Model             string `json:"model"`
		Format            string `json:"format"`
		Enabled           bool   `json:"enabled"`
		AutoTriage        bool   `json:"auto_triage"`
		TriageDailyBudget int32  `json:"triage_daily_budget"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TriageDailyBudget < 0 || req.TriageDailyBudget > 10000 {
		writeErr(w, http.StatusBadRequest, "triage_daily_budget must be between 0 and 10000")
		return
	}
	req.BaseURL = strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	req.Model = strings.TrimSpace(req.Model)
	req.APIKey = strings.TrimSpace(req.APIKey)
	if req.Format == "" {
		req.Format = "openai"
	}
	if req.Model == "" {
		writeErr(w, http.StatusBadRequest, "base_url (https) and model are required")
		return
	}
	// Validated at the trust boundary, not only at dial time. The runtime guard
	// blocks the connection, so this is not a breach; what it fixes is a stored
	// config the UI shows as working while every triage silently fails with
	// nothing saying why. It also closes the case the dial guard cannot cover:
	// FLARE_ALLOW_PRIVATE_AI_ENDPOINT turns that guard off process-wide, and
	// then there is no check at either layer.
	if err := netguard.ValidatePublicURL(req.BaseURL); err != nil && !s.allowPrivateAI {
		writeErr(w, http.StatusBadRequest, "base_url "+err.Error())
		return
	}
	if req.Format != "openai" && req.Format != "anthropic" {
		writeErr(w, http.StatusBadRequest, "format must be openai or anthropic")
		return
	}
	// The api_key column has two possible origins and they are NOT the same trust
	// level, so they take separate paths. A supplied key is client input and is
	// always encrypted. A blank key means "keep the stored one", and that value
	// is already encrypted at rest, so it is carried through untouched.
	//
	// Do not collapse these into a single Encrypt call that decides by looking at
	// the string. Letting a prefix on client input skip encryption is a
	// decryption oracle, and here it is an exfiltration primitive: base_url is
	// client-controlled too, so the decrypted victim key would be sent as a
	// Bearer token to a host the attacker picked.
	// "Omit the key to keep the stored one" is safe only while the DESTINATION
	// is unchanged. It was not gated on that, so an org admin, who by design can
	// never read the stored key back, could PUT a new base_url with no api_key,
	// call triage, and have Flare send the org's real provider key as a Bearer
	// token to a host they chose. The storage-layer decryption oracle was closed
	// and this front door was left open; netguard is no help because the
	// attacker's host is public.
	//
	// So changing where the credential is sent requires re-supplying the
	// credential, and the change is recorded as a security event either way.
	storedAPIKey := ""
	if req.APIKey == "" {
		existing, err := s.q.GetAIConfig(r.Context(), orgIDFrom(r.Context()))
		if err != nil || existing.ApiKey == "" {
			writeErr(w, http.StatusBadRequest, "an api key is required")
			return
		}
		if existing.BaseUrl != req.BaseURL {
			s.recordSecurityEvent(orgIDFrom(r.Context()), "ai-endpoint-change-without-key",
				"base_url changed from "+existing.BaseUrl+" to "+req.BaseURL+" with no api_key supplied",
				remoteIP(r))
			writeErr(w, http.StatusBadRequest,
				"changing base_url requires supplying the api key again")
			return
		}
		storedAPIKey = existing.ApiKey
	}
	apiKeyColumn := secretColumnForUpdate(s.secrets, req.APIKey, storedAPIKey)
	if err := s.q.UpsertAIConfig(r.Context(), generated.UpsertAIConfigParams{
		OrgID: orgIDFrom(r.Context()), BaseUrl: req.BaseURL, ApiKey: apiKeyColumn,
		Model: req.Model, Format: req.Format, Enabled: req.Enabled,
		AutoTriage: req.AutoTriage, TriageDailyBudget: req.TriageDailyBudget,
	}); err != nil {
		slogError(w, "save ai config", err)
		return
	}
	s.audit(r.Context(), "ai.configure", req.BaseURL)
	s.handleGetAIConfig(w, r)
}

func (s *Server) handleDeleteAIConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.q.DeleteAIConfig(r.Context(), orgIDFrom(r.Context())); err != nil {
		slogError(w, "delete ai config", err)
		return
	}
	s.audit(r.Context(), "ai.disconnect", "")
	writeJSON(w, http.StatusNoContent, nil)
}

// handleTriageIssue runs (or returns cached) AI triage for an issue: a plain
// language explanation, likely root cause, and suggested fix, built from the
// symbolicated stack trace. PII is scrubbed before it reaches the model.
// Sentinel errors so callers (the REST handler and the MCP tool) can map a
// triage failure to the right status/message without string-matching. The
// "not configured" text is load-bearing: the frontend and MCP surface a
// Settings hint when they see it.
var (
	errTriageNotConfigured = errors.New("AI triage is not configured for this workspace")
	errTriageEndpoint      = errors.New("the AI endpoint did not respond (check the base URL, key and model)")
)

func (s *Server) handleTriageIssue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org := orgIDFrom(ctx)
	issueID := chi.URLParam(r, "id")

	text, cached, evidenceDigest, aiReceiptID, err := s.triageIssue(ctx, org, issueID, r.URL.Query().Get("refresh") == "true")
	if err != nil {
		var nf telemetry.ErrNotFound
		switch {
		case errors.As(err, &nf):
			writeErr(w, http.StatusNotFound, "issue not found")
		case errors.Is(err, errTriageNotConfigured):
			writeErr(w, http.StatusBadRequest, errTriageNotConfigured.Error())
		case errors.Is(err, errTriageEndpoint):
			writeErr(w, http.StatusBadGateway, errTriageEndpoint.Error())
		case errors.Is(err, errTriageBudget):
			writeErr(w, http.StatusTooManyRequests, errTriageBudget.Error())
		case errors.Is(err, errAIActionReceiptUnavailable):
			writeErr(w, http.StatusServiceUnavailable, errAIActionReceiptUnavailable.Error())
		default:
			slogError(w, "ai triage", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, triageResponse(text, cached, evidenceDigest, aiReceiptID))
}

func triageResponse(text string, cached bool, evidenceDigest, aiReceiptID string) map[string]any {
	response := map[string]any{"triage": text, "cached": cached}
	addFreshTriageProvenance(response, cached, evidenceDigest, aiReceiptID)
	return response
}

// addFreshTriageProvenance is shared by REST and MCP so a cached result never
// inherits a previous run's proof, even when its evidence digest is unchanged.
func addFreshTriageProvenance(response map[string]any, cached bool, evidenceDigest, aiReceiptID string) {
	if cached || evidenceDigest == "" || aiReceiptID == "" {
		return
	}
	// A fresh triage receipt is the provenance handle an external coding
	// worker needs when it submits the human-gated patch receipt. Cached
	// triage deliberately has neither field and cannot authorize a patch.
	response["evidence_digest"] = evidenceDigest
	// This is the exact row inserted for this run. Completion is best effort,
	// so callers must read this row and require succeeded status.
	response["ai_receipt_id"] = aiReceiptID
}

// triageIssue runs (or returns cached) AI triage for one issue. Shared by the
// REST handler and the MCP tool. A fresh run returns the exact evidence digest
// stored in its durable receipt and its exact inserted row ID. Cached reads
// return neither and cannot authorize a patch. Receipt completion is best
// effort, so consumers must verify the returned ID's terminal status. The
// final value is a typed error (errTriageNotConfigured /
// errTriageEndpoint / telemetry.ErrNotFound / raw DB error).
func (s *Server) triageIssue(ctx context.Context, org, issueID string, refresh bool) (text string, cached bool, evidenceDigest, aiReceiptID string, retErr error) {
	issue, err := s.store.GetIssue(ctx, issueID, org)
	if err != nil {
		return "", false, "", "", err
	}
	receipt, receiptErr := s.requireAIActionReceipt(ctx, org, issue.ProjectID, issue.ID, "triage", "unknown")
	if receiptErr != nil {
		return "", false, "", "", receiptErr
	}
	defer func() {
		s.finishAIActionReceipt(ctx, receipt, cached, retErr)
		evidenceDigest, aiReceiptID = freshTriageProvenance(receipt, cached, retErr)
	}()
	if issue.AITriage != "" && !refresh {
		if receipt != nil {
			receipt.provider = "cache"
		}
		// Rows created before output egress scrubbing may contain a provider
		// echo of a credential or PII. Sanitize cached text before returning it
		// through REST or MCP, and repair the durable row when possible so a
		// later read cannot re-expose the old value.
		sanitized := sanitizeTriageOutput(issue.AITriage)
		if sanitized != issue.AITriage {
			if err := s.q.SetIssueTriage(ctx, generated.SetIssueTriageParams{
				ID: issue.ID, OrgID: org, AiTriage: sanitized,
			}); err != nil {
				slog.Warn("ai triage: unable to repair cached output", "issue", issue.ID, "org", org, "error", err)
			}
		}
		return sanitized, true, "", "", nil
	}

	cfg, err := s.q.GetAIConfig(ctx, org)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !cfg.Enabled) {
		return "", false, "", "", errTriageNotConfigured
	}
	if err != nil {
		return "", false, "", "", err
	}
	if receipt != nil {
		receipt.provider = canonicalAIActionProvider(cfg.Format)
	}

	// The BYOAI cost guard belongs HERE, not on the callers, because it kept
	// being written on some of them. Three surfaces reach this function: MCP
	// (rate-limited per org), auto-triage (budget-claimed), and
	// POST /issues/{id}/triage, which had neither. Any member, or any
	// member-scoped API key (which CI holds), could loop ?refresh=true and
	// spend the tenant's own OpenAI or Anthropic account one completion per
	// request, with TriageDailyBudget never consulted, so setting it to 1 did
	// not stop it. A retry loop in a deploy script was enough.
	//
	// Claimed before the outbound call and only when a call is actually going
	// to happen: a cached answer costs nothing and must not consume budget.
	if err := s.claimTriageBudget(ctx, org, cfg.TriageDailyBudget); err != nil {
		return "", false, "", "", err
	}

	// The report is written by whoever sent the event, not by us: a DSN public
	// key lives in a browser bundle, so the exception message and every stack
	// frame are anonymous input. Fence it and tell the model what the fence
	// means, because the answer does not stop at the dashboard: it is served
	// back through the MCP tools to the operator's own agent, which can write.
	report := s.buildTriageContext(ctx, issue, org)
	fenced, untrustedRule, err := ai.Fence(report)
	if err != nil {
		return "", false, "", "", fmt.Errorf("%w: %v", errTriageEndpoint, err)
	}
	// Keep only a deterministic digest of the bounded, scrubbed evidence in
	// the receipt. The random fence delimiter is transport framing, so hashing
	// report (the exact sanitized context inside it) keeps repeated reads of
	// unchanged incident evidence comparable without retaining prompt content.
	if receipt != nil {
		receipt.evidenceDigest = aiEvidenceDigest(report)
	}
	system := "You are a senior software engineer triaging a production error from an observability tool. " +
		"The stack trace has been de-minified to original source. Reply in concise GitHub-flavored markdown with three short sections: " +
		"**What happened** (plain language), **Likely root cause**, and **Suggested fix** (concrete, code-level where possible). " +
		"Only use what the report supports; do not invent details. " +
		untrustedRule

	// Fail closed. Returning the ciphertext on a decrypt failure sent
	// "x-api-key: enc:v1:<base64>" to the org's provider, putting the encrypted
	// form of their live credential in a third party's access log and breaking
	// every triage with no error naming why.
	apiKey, err := s.secrets.Decrypt(cfg.ApiKey)
	if err != nil {
		slog.Error("ai triage: stored provider key cannot be decrypted; check FLARE_SECRET_KEY", "org", org, "error", err)
		return "", false, "", "", errTriageNotConfigured
	}
	connectorCtx := integration.WithScope(ctx, integration.Scope{OrgID: org, ProjectID: issue.ProjectID})
	triage, err := s.ai.Complete(connectorCtx, ai.Config{BaseURL: cfg.BaseUrl, APIKey: apiKey, Model: cfg.Model, Format: cfg.Format}, system, fenced)
	if err != nil {
		return "", false, "", "", fmt.Errorf("%w: %v", errTriageEndpoint, err)
	}
	// The reply is untrusted too, and for a second reason on top of the
	// injected-telemetry one: base_url is org-configurable, so the endpoint
	// answering is not necessarily a model at all. Scrub credentials and PII,
	// then strip control characters and cap the size before the text is
	// persisted and re-served through the dashboard and MCP.
	triage = sanitizeTriageOutput(triage)
	if err := s.q.SetIssueTriage(ctx, generated.SetIssueTriageParams{ID: issue.ID, OrgID: org, AiTriage: triage}); err != nil {
		return "", false, "", "", err
	}
	// Scrubbed and capped. Every other egress site scrubs; this one wrote the
	// raw, unbounded issue title into audit_log, which handleListAuditLog then
	// serves to admins, so a leaked secret in an exception title survived in a
	// second place nobody was scrubbing.
	s.audit(ctx, "ai.triage", ai.Line(ai.Scrub(issue.Title)))
	return triage, false, "", "", nil
}

// freshTriageProvenance exposes one accepted run only after its provider call
// and issue update succeeded. The receipt may still be started if its final
// write failed; consumers must confirm that exact row reached succeeded.
func freshTriageProvenance(receipt *aiActionReceipt, cached bool, actionErr error) (digest, receiptID string) {
	if receipt == nil || cached || actionErr != nil || receipt.evidenceDigest == "" || receipt.id == "" {
		return "", ""
	}
	return receipt.evidenceDigest, receipt.id
}

// sanitizeTriageOutput applies the same egress scrubber used for telemetry
// before model output is persisted. A provider can echo a secret from the
// evidence or return arbitrary text (the endpoint is tenant-configurable), so
// Block alone is insufficient: it caps and flattens control characters but
// deliberately does not redact credentials or personal data.
func sanitizeTriageOutput(value string) string {
	return ai.Block(ai.Scrub(value))
}

// errTriageBudget is returned when the org has spent its BYOAI budget for the
// day. Caller-actionable and carrying no upstream detail, so mcpErrText passes
// it through like the other two triage sentinels.
var errTriageBudget = errors.New("this workspace has reached its AI triage budget for today")

// claimTriageBudget takes one unit of the org's daily BYOAI allowance,
// atomically. A budget of 0 means unlimited, which is the shipped default and
// what every install that never opened the setting has.
func (s *Server) claimTriageBudget(ctx context.Context, org string, budget int32) error {
	if budget <= 0 {
		return nil
	}
	if _, err := s.q.ClaimAITriageBudget(ctx, generated.ClaimAITriageBudgetParams{
		OrgID: org, Budget: budget,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errTriageBudget
		}
		return err
	}
	return nil
}

// maybeAutoTriage runs AI triage on a newly-seen issue in the background when
// the org has auto-triage enabled and is under its daily budget. Fire-and-forget
// with a detached context: it must never block or fail ingest. The budget is
// claimed atomically BEFORE the model call, so a burst of new fingerprints
// cannot run more than triage_daily_budget BYOAI completions in a day.
func (s *Server) maybeAutoTriage(org, issueID string) {
	s.goBackgroundFor(org, "auto-triage", 90*time.Second, func(ctx context.Context) {
		cfg, err := s.q.GetAIConfig(ctx, org)
		if err != nil || !cfg.Enabled || !cfg.AutoTriage || cfg.TriageDailyBudget <= 0 {
			return
		}
		// The claim lives inside triageIssue now, so this path no longer takes
		// its own. Claiming here as well would spend two units per new issue.
		if _, _, _, _, err := s.triageIssue(ctx, org, issueID, false); err != nil {
			switch {
			case errors.Is(err, errTriageBudget):
				slog.Info("auto-triage skipped: daily budget reached", "org", org, "budget", cfg.TriageDailyBudget)
			default:
				slog.Warn("auto-triage failed", "issue", issueID, "error", err)
			}
		}
	})
}

type triageFrame struct {
	Filename    string `json:"filename"`
	Function    string `json:"function"`
	Lineno      int    `json:"lineno"`
	ContextLine string `json:"context_line"`
}

// buildTriageContext assembles a scrubbed, model-ready description of the issue
// from its latest event's symbolicated stack trace. PII is scrubbed last so
// nothing personal leaves the tenant boundary.
func (s *Server) buildTriageContext(ctx context.Context, issue telemetry.Issue, org string) string {
	var b strings.Builder
	// Every value below arrives on the ingest endpoint, so each one is flattened
	// to a single line first. Otherwise a title of "boom\n\nLocation: ..." lets
	// the sender forge additional report sections, which is the cheapest version
	// of the injection: text that reads as though Flare wrote it.
	fmt.Fprintf(&b, "Error: %s\n", ai.Line(issue.Title))
	fmt.Fprintf(&b, "Level: %s   Release: %s\n", ai.Line(issue.Level), ai.Line(issue.FirstRelease))
	if issue.Culprit != "" {
		fmt.Fprintf(&b, "Location: %s\n", ai.Line(issue.Culprit))
	}

	events, _ := s.store.ListEventsByIssue(ctx, issue.ID, org, 1)
	if len(events) > 0 {
		e := events[0]
		if e.ExceptionType != "" || e.ExceptionValue != "" {
			fmt.Fprintf(&b, "Exception: %s: %s\n", ai.Line(e.ExceptionType), ai.Line(e.ExceptionValue))
		}
		stack := e.Stacktrace
		if maps := s.releaseSourceMaps(ctx, issue.ID, org, events)[e.Release]; len(maps) > 0 {
			stack = s.symbolicator.Symbolicate(stack, maps)
		}
		if status := s.releaseNativeSymbolStatuses(ctx, issue.ID, org, events)[e.Release]; status != "" {
			// This is an evidence boundary, not a claim that the model can read
			// native frames. Keep the unresolved state in the prompt so a triage
			// answer does not describe a Dart/native frame as source-resolved.
			fmt.Fprintf(&b, "Native symbols: %s (native frame rewriting is not enabled)\n", ai.Line(status))
		}
		var st struct {
			Frames []triageFrame `json:"frames"`
		}
		if json.Unmarshal(stack, &st) == nil && len(st.Frames) > 0 {
			b.WriteString("\nStack trace (most recent call last):\n")
			frames := st.Frames
			if len(frames) > 30 {
				frames = frames[len(frames)-30:]
			}
			for _, f := range frames {
				fn := ai.Line(f.Function)
				if fn == "" {
					fn = "?"
				}
				fmt.Fprintf(&b, "  %s at %s:%d\n", fn, ai.Line(f.Filename), f.Lineno)
				if cl := ai.Line(f.ContextLine); cl != "" {
					fmt.Fprintf(&b, "      > %s\n", cl)
				}
			}
		}
	}
	// Scrub last so PII never leaves the tenant boundary, then cap the whole
	// region: 30 frames of attacker-sized context lines would otherwise be an
	// unbounded prompt on the tenant's own BYOAI key.
	return ai.Block(ai.Scrub(b.String()))
}
