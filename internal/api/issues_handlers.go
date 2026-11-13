package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/ingest"
	"github.com/bright-interaction/flare/internal/native_symbols"
	"github.com/bright-interaction/flare/internal/scan"
	"github.com/bright-interaction/flare/internal/telemetry"
)

type issueResponse struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	Title        string    `json:"title"`
	Culprit      string    `json:"culprit"`
	Level        string    `json:"level"`
	Status       string    `json:"status"`
	Platform     string    `json:"platform"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	EventCount   int64     `json:"event_count"`
	GithubURL    string    `json:"github_url"`
	FirstRelease string    `json:"first_release"`
	AITriage     string    `json:"ai_triage"`
	Sensitive    string    `json:"sensitive"`
}

func toIssueResponse(i telemetry.Issue) issueResponse {
	out := issueResponse{
		ID: i.ID, ProjectID: i.ProjectID, Title: i.Title, Culprit: i.Culprit, Level: i.Level,
		Status: i.Status, Platform: i.Platform,
		FirstSeen: i.FirstSeen, LastSeen: i.LastSeen, EventCount: i.EventCount,
		GithubURL: i.GithubURL, FirstRelease: i.FirstRelease, AITriage: i.AITriage,
		Sensitive: i.Sensitive,
	}
	redactIfFlagged(&out, i.Sensitive)
	return out
}

// genIssueToResponse maps the write-path row (UpdateIssueStatus returns the
// generated type) into the same response shape.
func genIssueToResponse(i *generated.Issue) issueResponse {
	out := issueResponse{
		ID: i.ID, ProjectID: i.ProjectID, Title: i.Title, Culprit: i.Culprit, Level: i.Level,
		Status: i.Status, Platform: i.Platform,
		FirstSeen: i.FirstSeen.Time, LastSeen: i.LastSeen.Time, EventCount: i.EventCount,
		GithubURL: i.GithubUrl, FirstRelease: i.FirstRelease, AITriage: i.AiTriage,
		Sensitive: i.Sensitive,
	}
	redactIfFlagged(&out, i.Sensitive)
	return out
}

// redactIfFlagged scrubs a response IN FULL when the detector flagged this
// issue's telemetry, rather than picking two fields out of it.
//
// Picking fields is what made this the audit's only CRITICAL. detectSensitive
// scans the message, the exception type and value, the culprit AND every
// frame's context line; the REST response scrubbed Title and Culprit. So the
// single most common way the flag fires, a JWT in a source context line,
// produced an issue with a "sensitive data" badge whose flagged value was
// served verbatim by the endpoint the badge exists to protect, down to role
// viewer and to every read-only API key. AITriage is model output derived from
// that same value and was equally raw.
//
// The set of fields scanned and the set of fields redacted now come from the
// same place: the struct itself.
func redactIfFlagged(v any, sensitive string) {
	if sensitive == "" {
		return
	}
	scan.Struct(v)
}

type eventResponse struct {
	ID             string `json:"id"`
	Level          string `json:"level"`
	Message        string `json:"message"`
	ExceptionType  string `json:"exception_type"`
	ExceptionValue string `json:"exception_value"`
	Platform       string `json:"platform"`
	Environment    string `json:"environment"`
	Release        string `json:"release"`
	// NativeSymbolStatus is intentionally an explicit unresolved state. An
	// uploaded Flutter/Dart artifact is retained and tenant-scoped, but native
	// frame rewriting is not enabled by this read path yet.
	NativeSymbolStatus string          `json:"native_symbol_status,omitempty"`
	Stacktrace         json.RawMessage `json:"stacktrace"`
	TraceID            string          `json:"trace_id"`
	SpanID             string          `json:"span_id"`
	Tags               []eventTag      `json:"tags,omitempty"`
	ReceivedAt         time.Time       `json:"received_at"`
}

func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID := chi.URLParam(r, "id")
	org := orgIDFrom(ctx)
	limit, offset := parsePaging(r)

	var status *string
	if v := r.URL.Query().Get("status"); v != "" {
		status = &v
	}
	q := searchTerm(r.URL.Query().Get("q"))

	issues, err := s.store.ListIssues(ctx, projectID, org, limit, offset, status, q)
	if err != nil {
		slogError(w, "list issues", err)
		return
	}
	// Same filter as the list, so the total always describes what is shown.
	total, err := s.store.CountIssues(ctx, projectID, org, status, q)
	if err != nil {
		slogError(w, "count issues", err)
		return
	}

	out := make([]issueResponse, 0, len(issues))
	for _, i := range issues {
		out = append(out, toIssueResponse(i))
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": out, "total": total})
}

func (s *Server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	i, err := s.store.GetIssue(r.Context(), chi.URLParam(r, "id"), orgIDFrom(r.Context()))
	if err != nil {
		var nf telemetry.ErrNotFound
		if errors.As(err, &nf) {
			writeErr(w, http.StatusNotFound, "issue not found")
			return
		}
		slogError(w, "get issue", err)
		return
	}
	writeJSON(w, http.StatusOK, toIssueResponse(i))
}

func (s *Server) handleListIssueEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org := orgIDFrom(ctx)
	issueID := chi.URLParam(r, "id")
	events, err := s.store.ListEventsByIssue(ctx, issueID, org, 50)
	if err != nil {
		slogError(w, "list issue events", err)
		return
	}
	// If the issue is flagged sensitive, scrub the leaked value out of the
	// event text before returning it.
	//
	// Fails CLOSED on anything except a genuine not-found. Discarding the error
	// left issue.Sensitive as "" and returned the events unscrubbed, so the
	// PERMISSIVE branch was the error branch: a transient database blip served
	// the flagged value in full. A missing issue means there are no events to
	// scrub, so that one case is safe to treat as unflagged.
	issue, err := s.store.GetIssue(ctx, issueID, org)
	sensitive := issue.Sensitive != ""
	if err != nil {
		var nf telemetry.ErrNotFound
		if !errors.As(err, &nf) {
			sensitive = true
		}
	}
	writeJSON(w, http.StatusOK, s.toEventResponses(ctx, issueID, org, events, sensitive))
}

// toEventResponses maps stored events to the clean API shape, symbolicating
// minified frames using any source maps uploaded for each event's release
// (best-effort: distinct releases are loaded once, frames left untouched on a
// miss). When sensitive is set (the issue's detector flagged a secret/PII
// value), the WHOLE event is scrubbed, stack trace included: the detector reads
// frame context lines, so leaving the stack raw served the flagged value from
// the endpoint the flag exists to protect. Tags are scrubbed whether or not the
// issue is flagged: they were not scanned on ingest until v0.6.7, so the flag says
// nothing about the tags of an event stored before then. Shared by the REST events
// endpoint and the MCP get_issue tool.
func (s *Server) toEventResponses(ctx context.Context, issueID, org string, events []telemetry.Event, sensitive bool) []eventResponse {
	releaseMaps := s.releaseSourceMaps(ctx, issueID, org, events)
	nativeStatuses := s.releaseNativeSymbolStatuses(ctx, issueID, org, events)
	out := make([]eventResponse, 0, len(events))
	for _, e := range events {
		stack := e.Stacktrace
		if maps := releaseMaps[e.Release]; len(maps) > 0 {
			stack = s.symbolicator.Symbolicate(stack, maps)
		}
		out = append(out, eventResponse{
			ID: e.ID, Level: e.Level, Message: e.Message,
			ExceptionType: e.ExceptionType, ExceptionValue: e.ExceptionValue,
			Platform: e.Platform, Environment: e.Environment, Release: e.Release,
			NativeSymbolStatus: nativeStatuses[e.Release],
			Stacktrace:         stack, TraceID: e.TraceID, SpanID: e.SpanID, Tags: scan.Pairs(eventTags(e.Payload)), ReceivedAt: e.ReceivedAt,
		})
	}
	if sensitive {
		scan.Struct(out)
	}
	return out
}

// releaseNativeSymbolStatuses reports only what the metadata query proves: an
// artifact exists for this tenant/project/release, while format-specific native
// frame rewriting remains unresolved. It deliberately never selects artifact
// bytes, and it fails closed to an empty status on a read error so issue detail
// stays available without inventing symbolication.
func (s *Server) releaseNativeSymbolStatuses(ctx context.Context, issueID, org string, events []telemetry.Event) map[string]string {
	if s == nil || s.q == nil || s.store == nil || len(events) == 0 {
		return nil
	}
	releases := make(map[string]struct{}, len(events))
	for _, e := range events {
		if e.Release != "" {
			releases[e.Release] = struct{}{}
		}
	}
	if len(releases) == 0 {
		return nil
	}
	issue, err := s.store.GetIssue(ctx, issueID, org)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(releases))
	for release := range releases {
		rows, err := s.q.ListNativeSymbolArtifactMetadataForRelease(ctx, generated.ListNativeSymbolArtifactMetadataForReleaseParams{
			ProjectID: issue.ProjectID,
			OrgID:     org,
			Release:   release,
			RowLimit:  64,
		})
		if err != nil || len(rows) == 0 {
			continue
		}
		// Keep this label honest for every currently retained format. A future
		// resolver can replace it with a format-specific recognized state after
		// validating bytes; today's metadata read must not imply that.
		out[release] = string(native_symbols.StatusRetainedUnresolved)
	}
	return out
}

// scrubIssueForMCP scrubs the issue for the LLM boundary. It runs
// UNCONDITIONALLY, unlike toIssueResponse, which only scrubs when the detector
// already flagged the issue as sensitive. The detector is a heuristic: a title is
// the raw "ExceptionType: ExceptionValue" from the reporting service, so it can
// carry PII/secrets the detector never matched. REST keeps the flag-conditional
// behaviour, because there the reader is an authenticated operator of that org.
//
// It scrubs by STRUCTURE. The hand-picked list it replaces covered five fields
// and omitted Level and GithubURL, and its event sibling omitted Level,
// Platform, TraceID and SpanID, every one of them client-written with no enum
// check anywhere upstream.
func scrubIssueForMCP(i issueResponse) issueResponse {
	scan.Struct(&i)
	return i
}

// scrubEventsForMCP is the same rule for the events that cross the LLM boundary
// via get_issue: frame context lines and locals routinely carry request params,
// DSNs and secrets from the reporting service. Applied ONLY on the MCP path;
// the REST dashboard shows the raw event to the authenticated operator of that
// org unless the issue is flagged.
func scrubEventsForMCP(events []eventResponse) []eventResponse {
	scan.Struct(events)
	return events
}

// releaseSourceMaps loads the source maps (name -> content) for each distinct
// release across the issue's events, keyed by release. Returns an empty map on
// any error so symbolication simply no-ops.
func (s *Server) releaseSourceMaps(ctx context.Context, issueID, org string, events []telemetry.Event) map[string]map[string]string {
	releases := map[string]bool{}
	for _, e := range events {
		if e.Release != "" {
			releases[e.Release] = true
		}
	}
	if len(releases) == 0 {
		return nil
	}
	issue, err := s.store.GetIssue(ctx, issueID, org)
	if err != nil {
		return nil
	}
	// Byte budget for the WHOLE request, spent across releases.
	//
	// A row cap does not bound memory here: an artifact may be 30 MiB
	// (maxArtifactBody), so the old LIMIT 1000 allowed 30 GB per release, and an
	// issue whose events span several releases multiplied that again. One
	// GET /api/issues/{id}/events was enough to OOM the shared process, which on
	// a self-host takes down ingest for every tenant with it.
	//
	// 64 MiB comfortably covers a real bundle's maps while making the endpoint's
	// worst case bounded and predictable.
	const maxSourceMapBytes = 64 << 20
	remaining := int64(maxSourceMapBytes)

	out := make(map[string]map[string]string, len(releases))
	for rel := range releases {
		if remaining <= 0 {
			break
		}
		rows, err := s.q.GetSourceMapsForRelease(ctx, generated.GetSourceMapsForReleaseParams{
			ProjectID: issue.ProjectID, OrgID: org, Release: rel,
			MaxBytes: remaining,
		})
		if err != nil || len(rows) == 0 {
			continue
		}
		m := make(map[string]string, len(rows))
		for _, row := range rows {
			m[row.Name] = row.Content
			remaining -= int64(len(row.Content))
		}
		out[rel] = m
	}
	return out
}

func (s *Server) handleUpdateIssueStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status string `json:"status"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch req.Status {
	case "unresolved", "resolved", "ignored":
	default:
		writeErr(w, http.StatusBadRequest, "status must be unresolved, resolved, or ignored")
		return
	}
	i, err := s.q.UpdateIssueStatus(r.Context(), generated.UpdateIssueStatusParams{
		ID: chi.URLParam(r, "id"), OrgID: orgIDFrom(r.Context()), Status: req.Status,
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "issue not found")
		return
	}
	writeJSON(w, http.StatusOK, genIssueToResponse(i))
}

// searchTerm turns a raw ?q= into the value handed to a query, or nil for an
// empty one. Three guards, always together, for every search surface:
//
//   - truncate to maxSearchTermBytes, rune-safe. A raw v[:200] byte slice
//     splits a multi-byte character and hands Postgres invalid UTF-8, which
//     errors the whole query.
//   - SanitizeText, so ?q=%00 cannot reach pgx as invalid UTF-8 and 500 the
//     endpoint for anyone with dashboard access.
//   - escapeLike, so an operator searching for "50%" or "user_id" gets the
//     rows they asked for instead of silently wrong ones.
//
// It exists because those three were applied to the issue search term and none
// of them to the log search term, even though the commit that wired log search
// says in its own message "six surfaces in one commit, per the repo's
// incomplete-migration rule". One helper, four call sites, and a ciguard rule
// that fails CI on an ILIKE without its ESCAPE clause.
func searchTerm(raw string) *string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil
	}
	v = escapeLike(ingest.SanitizeText(truncateRunes(v, maxSearchTermBytes)))
	return &v
}

// maxSearchTermBytes bounds a search term. The logs surface accepted 5000 bytes
// while its issues twin capped at 200.
const maxSearchTermBytes = 200

// escapeLike neutralises LIKE/ILIKE metacharacters so a search term is matched
// literally. The queries declare ESCAPE '\', so the backslash must be escaped
// first or it would swallow the character after it.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// truncateRunes cuts s to at most n bytes without splitting a UTF-8 rune.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// maxOffset ceilings OFFSET paging. An unbounded offset reaches
// events.sql's `OFFSET $4`, and Postgres computes and discards every row up to
// it, so ?offset=2000000000 is a full scan the caller pays nothing for. Nobody
// pages 100 issues at a time to row 500,000; beyond this the answer is a
// filter, not another page.
const maxOffset = 10000

func parsePaging(r *http.Request) (limit, offset int32) {
	limit, offset = 50, 0
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 100 {
		limit = int32(v)
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v >= 0 {
		if v > maxOffset {
			v = maxOffset
		}
		offset = int32(v)
	}
	return limit, offset
}
