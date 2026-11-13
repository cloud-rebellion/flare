package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/analytics"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/partition"
	"github.com/bright-interaction/flare/internal/retention"
)

// Data retention settings.
//
// The policy is INSTANCE-WIDE: the telemetry partitions and the Parquet archive
// are shared by every org, so one org's admin choosing 7 days decides for every
// tenant on the instance. That is why the routes sit behind requireRole("admin")
// (no API key can reach admin) AND requireOperatorWorkspace: on a multi-tenant
// instance only admins of the operator workspace (orgs.is_operator, migration
// 093: the first org created, or the oldest one at upgrade) may read or change
// it. On a single-workspace self-host, that is simply "an admin".

// SetRetentionStatusSource lets the settings page show what the last
// retention pass did. Called once by main before the HTTP server starts.
func (s *Server) SetRetentionStatusSource(fn func() partition.Status) {
	s.retentionStatus = fn
}

func (s *Server) lastRetentionStatus() *partition.Status {
	fn := s.retentionStatus
	if fn == nil {
		return nil
	}
	st := fn()
	if st.LastPassAt.IsZero() {
		return nil
	}
	return &st
}

// envRetention is the policy the environment gives when nothing is stored.
func (s *Server) envRetention() retention.Policy {
	p := retention.Policy{HotDays: s.cfg.RetentionDays, ColdMode: s.cfg.ColdRetentionMode, ColdDays: s.cfg.ColdRetentionDays}
	if p.HotDays <= 0 {
		p.HotDays = 30 // partition.New's own default
	}
	if p.ColdMode == "" {
		p.ColdMode = retention.ColdForever
		p.ColdDays = 0
	}
	return p
}

func storedPolicy(row *generated.GetRetentionSettingsRow) *retention.Policy {
	if row == nil {
		return nil
	}
	p := retention.Policy{HotDays: int(row.HotRetentionDays), ColdMode: retention.ColdMode(row.ColdMode)}
	if row.ColdRetentionDays.Valid {
		p.ColdDays = int(row.ColdRetentionDays.Int32)
	}
	return &p
}

// effectiveRetention resolves the stored-or-env policy. The stored row is
// returned too so callers can show who changed it.
func (s *Server) effectiveRetention(ctx context.Context) (retention.Policy, retention.Source, *generated.GetRetentionSettingsRow, error) {
	row, err := s.q.GetRetentionSettings(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = nil, nil
	}
	if err != nil {
		return retention.Policy{}, "", nil, err
	}
	p, src, err := retention.Resolve(storedPolicy(row), s.envRetention())
	return p, src, row, err
}

// RetentionPolicy is the partition manager's PolicyFunc: read on every pass, so
// a saved change applies on the next pass with no restart. Any error makes the
// pass prune nothing.
func (s *Server) RetentionPolicy(ctx context.Context) (retention.Policy, error) {
	if s.q == nil {
		return s.envRetention(), nil
	}
	p, _, _, err := s.effectiveRetention(ctx)
	return p, err
}

// retentionPosture is effective retention as every reporting surface must see
// it: readiness, analytics health, source-map retirement and the settings
// view. The retention fields of s.cfg are env fallbacks, never effective
// values on their own; only envRetention and postureFor read them, and
// internal/ciguard fails the build on a read anywhere else.
type retentionPosture struct {
	Policy retention.Policy
	// Known is false when the stored policy could not be read. The partition
	// manager then drops nothing, so surfaces report retention as paused.
	Known bool
	// AllowDropWithoutExport is FLARE_ALLOW_DROP_WITHOUT_EXPORT: with the cold
	// tier down, aged partitions are dropped unarchived instead of kept.
	AllowDropWithoutExport bool
}

func (s *Server) retentionPosture(ctx context.Context) retentionPosture {
	p, err := s.RetentionPolicy(ctx)
	if err != nil {
		slog.Warn("retention policy unavailable for status reporting", "error", err)
		return s.postureFor(s.envRetention(), false)
	}
	return s.postureFor(p, true)
}

// postureFor is, with envRetention, the only code that reads the retention
// fields of s.cfg (internal/ciguard enforces it by function name).
func (s *Server) postureFor(p retention.Policy, known bool) retentionPosture {
	return retentionPosture{Policy: p, Known: known, AllowDropWithoutExport: s.cfg.AllowDropWithoutExport}
}

// dropsUnarchived reports whether aged partitions leave Postgres with no
// Parquet copy: archiving is off by choice, or the cold tier is down and the
// operator allowed dropping without export.
func (p retentionPosture) dropsUnarchived(analyticsConfigured bool) bool {
	if !p.Known {
		return false
	}
	return !p.Policy.Archives() || (!analyticsConfigured && p.AllowDropWithoutExport)
}

// CheckOperatorOrg logs an error when workspaces exist but none is the
// operator (orgs.is_operator). Instance-wide settings are then unmanageable
// until someone flags one by hand; registration will not hand the flag out.
// Called once at boot; the error is returned for callers that want to refuse.
func (s *Server) CheckOperatorOrg(ctx context.Context) (missing bool, err error) {
	v, err := s.q.OperatorOrgMissing(ctx)
	missing = v.Valid && v.Bool
	if err != nil {
		slog.Warn("operator workspace check failed", "error", err)
		return false, err
	}
	if missing {
		slog.Error("no operator workspace: workspaces exist but none has orgs.is_operator set, so data retention cannot be managed from the dashboard. Flag one: UPDATE orgs SET is_operator = TRUE WHERE id = '<org id>'")
	}
	return missing, nil
}

// requireOperatorWorkspace refuses unless the caller's org is the operator
// workspace. Fails closed when that cannot be determined.
func (s *Server) requireOperatorWorkspace(w http.ResponseWriter, r *http.Request) bool {
	op, err := s.q.GetOperatorOrg(r.Context())
	if err != nil {
		slog.Warn("retention settings: operator workspace lookup failed", "error", err)
		writeErr(w, http.StatusForbidden, "data retention is instance-wide and is managed from the operator workspace")
		return false
	}
	if op.ID != orgIDFrom(r.Context()) {
		writeErr(w, http.StatusForbidden, "data retention is instance-wide and is managed from the operator workspace")
		return false
	}
	return true
}

type retentionBounds struct {
	HotMin  int `json:"hot_min"`
	HotMax  int `json:"hot_max"`
	ColdMax int `json:"cold_max"`
}

type retentionColdTier struct {
	// Kind is local, s3, or unavailable (DuckDB did not open).
	Kind           string `json:"kind"`
	PruneSupported bool   `json:"prune_supported"`
	Note           string `json:"note,omitempty"`
}

type retentionDay struct {
	Day   string `json:"day"`
	Rows  int64  `json:"approx_rows"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files,omitempty"`
}

type retentionResponse struct {
	Effective  retention.Policy  `json:"effective"`
	Source     retention.Source  `json:"source"`
	EnvDefault retention.Policy  `json:"env_default"`
	Stored     *retention.Policy `json:"stored"`
	UpdatedAt  *time.Time        `json:"updated_at"`
	UpdatedBy  string            `json:"updated_by,omitempty"`
	Bounds     retentionBounds   `json:"bounds"`
	// Today is the UTC day the server computes cutoffs from, so the preview
	// in the browser uses the same calendar as the partition manager.
	Today    string            `json:"today"`
	ColdTier retentionColdTier `json:"cold_tier"`
	// AllowDropWithoutExport mirrors FLARE_ALLOW_DROP_WITHOUT_EXPORT. With the
	// cold tier unavailable it means aged days are DELETED, not archived, and
	// the preview must say so.
	AllowDropWithoutExport bool `json:"allow_drop_without_export"`
	// HotPausedReason is set when the hot prune cannot run as configured.
	HotPausedReason string            `json:"hot_paused_reason,omitempty"`
	Hot             []retentionDay    `json:"hot"`
	Cold            []retentionDay    `json:"cold"`
	LastPass        *partition.Status `json:"last_pass"`
}

func (s *Server) retentionView(ctx context.Context) (*retentionResponse, error) {
	eff, src, row, err := s.effectiveRetention(ctx)
	if err != nil {
		return nil, err
	}
	posture := s.postureFor(eff, true)
	out := &retentionResponse{
		AllowDropWithoutExport: posture.AllowDropWithoutExport,
		Effective:              eff, Source: src, EnvDefault: s.envRetention(), Stored: storedPolicy(row),
		Bounds:   retentionBounds{HotMin: retention.MinHotDays, HotMax: retention.MaxHotDays, ColdMax: retention.MaxColdDays},
		Today:    retention.Today(time.Now()).Format("2006-01-02"),
		LastPass: s.lastRetentionStatus(),
		Hot:      []retentionDay{}, Cold: []retentionDay{},
	}
	if row != nil {
		if row.UpdatedAt.Valid {
			t := row.UpdatedAt.Time
			out.UpdatedAt = &t
		}
		out.UpdatedBy = row.UpdatedByEmail.String
	}

	switch {
	case s.analytics == nil:
		out.ColdTier = retentionColdTier{Kind: "unavailable", Note: "The analytics engine (DuckDB) did not start, so the Parquet archive cannot be exported to, read or pruned."}
		switch {
		case posture.dropsUnarchived(false) && eff.Archives():
			out.HotPausedReason = "The cold tier is down and FLARE_ALLOW_DROP_WITHOUT_EXPORT is set, so aged partitions are deleted without an archive."
		case eff.Archives():
			out.HotPausedReason = "Aged partitions are kept because they cannot be archived while the cold tier is down."
		}
	case !s.analytics.ColdPruneSupported():
		out.ColdTier = retentionColdTier{Kind: s.analytics.ColdKind(), Note: "Flare cannot delete objects from an S3 archive. Use a bucket lifecycle rule on the dt= prefixes to expire it."}
	default:
		out.ColdTier = retentionColdTier{Kind: "local", PruneSupported: true}
	}

	hot, err := partition.HotInventory(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	out.Hot = aggregateHot(hot)
	if out.ColdTier.PruneSupported {
		cold, err := s.analytics.ColdInventory(ctx)
		if err != nil {
			// The archive listing is advisory. Say so instead of failing the page.
			slog.Warn("retention settings: cold inventory failed", "error", err)
			out.ColdTier.Note = "The archive could not be listed, so its size is unknown."
		} else {
			out.Cold = aggregateCold(cold)
		}
	}
	return out, nil
}

func aggregateHot(in []partition.DayUsage) []retentionDay {
	byDay := map[string]*retentionDay{}
	for _, u := range in {
		k := u.Day.Format("2006-01-02")
		d := byDay[k]
		if d == nil {
			d = &retentionDay{Day: k}
			byDay[k] = d
		}
		d.Rows += u.Rows
		d.Bytes += u.Bytes
	}
	return sortedDays(byDay)
}

func aggregateCold(in []analytics.ColdDayUsage) []retentionDay {
	byDay := map[string]*retentionDay{}
	for _, u := range in {
		k := u.Day.Format("2006-01-02")
		d := byDay[k]
		if d == nil {
			d = &retentionDay{Day: k}
			byDay[k] = d
		}
		d.Bytes += u.Bytes
		d.Files += u.Files
	}
	return sortedDays(byDay)
}

func sortedDays(m map[string]*retentionDay) []retentionDay {
	out := make([]retentionDay, 0, len(m))
	for _, d := range m {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

func (s *Server) handleGetRetention(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperatorWorkspace(w, r) {
		return
	}
	view, err := s.retentionView(r.Context())
	if err != nil {
		slogError(w, "retention settings", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// retentionExpected is the state the admin's decision was made against: the
// effective policy and the stored row's updated_at (null when nothing was
// stored) from the view the browser loaded. A write whose expected state no
// longer matches is refused with 409 and the fresh view, so a confirm given
// against one policy is never applied on top of another.
type retentionExpected struct {
	Effective retention.Policy `json:"effective"`
	UpdatedAt *time.Time       `json:"updated_at"`
}

func (e *retentionExpected) matches(cur retention.Policy, updatedAt pgtype.Timestamptz) bool {
	if e == nil {
		return true
	}
	if e.Effective != cur {
		return false
	}
	if e.UpdatedAt == nil || !updatedAt.Valid {
		return e.UpdatedAt == nil && !updatedAt.Valid
	}
	return e.UpdatedAt.Equal(updatedAt.Time)
}

type retentionRequest struct {
	HotRetentionDays  int    `json:"hot_retention_days"`
	ColdMode          string `json:"cold_mode"`
	ColdRetentionDays *int   `json:"cold_retention_days"`
	// ConfirmReduction must be true when the change keeps less data in either
	// tier than the current effective policy. The UI sets it from its confirm
	// step; an API caller has to say it too.
	ConfirmReduction bool `json:"confirm_reduction"`
	// Expected is optional for API callers and always sent by the UI.
	Expected *retentionExpected `json:"expected"`
}

// retentionResetRequest is the optional body of DELETE. confirm_reduction is
// also accepted as a query parameter.
type retentionResetRequest struct {
	ConfirmReduction bool               `json:"confirm_reduction"`
	Expected         *retentionExpected `json:"expected"`
}

// retentionConflict is the 409 body: why, plus the current state so the
// client can re-render its confirm from fresh data instead of a stale view.
type retentionConflict struct {
	Error   string             `json:"error"`
	Current *retentionResponse `json:"current,omitempty"`
}

func (req retentionRequest) policy() retention.Policy {
	p := retention.Policy{HotDays: req.HotRetentionDays, ColdMode: retention.ColdMode(req.ColdMode)}
	if req.ColdRetentionDays != nil {
		p.ColdDays = *req.ColdRetentionDays
	}
	return p
}

func (s *Server) handleSetRetention(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperatorWorkspace(w, r) {
		return
	}
	var req retentionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	next := req.policy()
	if err := next.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.changeRetention(w, r, &next, req.ConfirmReduction, req.Expected)
}

// handleResetRetention deletes the stored policy so the environment default
// applies again. Confirmation is required when the env default keeps less.
func (s *Server) handleResetRetention(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperatorWorkspace(w, r) {
		return
	}
	var req retentionResetRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	confirm := req.ConfirmReduction || r.URL.Query().Get("confirm_reduction") == "true"
	s.changeRetention(w, r, nil, confirm, req.Expected)
}

// changeRetention is the ONE write path for the policy. next nil means reset
// to the environment default. In a single transaction it takes the write
// lock, reads the current row FOR UPDATE, checks the client's expected state
// and the reduction confirm against THAT row, writes, and inserts the audit
// row; an audit failure rolls the change back. Two admins saving at once are
// serialised, so neither can slip an unconfirmed reduction past the other.
func (s *Server) changeRetention(w http.ResponseWriter, r *http.Request, next *retention.Policy, confirm bool, expected *retentionExpected) {
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		slogError(w, "retention settings: begin", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)
	if err := qtx.LockRetentionSettings(ctx); err != nil {
		slogError(w, "retention settings: lock", err)
		return
	}
	row, err := qtx.GetRetentionSettingsForUpdate(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = nil, nil
	}
	if err != nil {
		slogError(w, "retention settings: read current", err)
		return
	}
	var stored *retention.Policy
	var updatedAt pgtype.Timestamptz
	if row != nil {
		locked := generated.GetRetentionSettingsRow(*row)
		stored, updatedAt = storedPolicy(&locked), row.UpdatedAt
	}
	cur, src, err := retention.Resolve(stored, s.envRetention())
	if err != nil {
		slogError(w, "retention settings: read current", err)
		return
	}
	if !expected.matches(cur, updatedAt) {
		_ = tx.Rollback(ctx)
		s.retentionConflict(w, r, "retention was changed since this page loaded; review the current policy and try again")
		return
	}
	env := s.envRetention()
	target, action, auditTarget := env, "retention.reset", ""
	if next != nil {
		target, action = *next, "retention.update"
		auditTarget = cur.Describe() + " -> " + next.Describe()
	} else {
		auditTarget = cur.Describe() + " -> env default " + env.Describe()
	}
	if next == nil && src != retention.SourceStored {
		// Nothing stored, nothing to reset: answer with the view, write no row.
		_ = tx.Rollback(ctx)
		s.writeRetentionView(w, r)
		return
	}
	if retention.Reduces(cur, target) && !confirm {
		_ = tx.Rollback(ctx)
		s.retentionConflict(w, r, retention.ErrReductionNotConfirmed.Error())
		return
	}
	actor := pgtype.Text{}
	if uid := userIDFrom(ctx); uid != "" {
		actor = pgText(uid)
	}
	if next != nil {
		cold := pgtype.Int4{}
		if next.ColdMode == retention.ColdDays {
			cold = pgtype.Int4{Int32: int32(next.ColdDays), Valid: true}
		}
		err = qtx.UpsertRetentionSettings(ctx, generated.UpsertRetentionSettingsParams{
			HotRetentionDays: int32(next.HotDays), ColdMode: string(next.ColdMode), ColdRetentionDays: cold, UpdatedBy: actor,
		})
	} else {
		_, err = qtx.DeleteRetentionSettings(ctx)
	}
	if err != nil {
		slogError(w, "retention settings: save", err)
		return
	}
	if err := qtx.InsertAuditLog(ctx, generated.InsertAuditLogParams{
		ID: id.New(), OrgID: orgIDFrom(ctx), ActorUserID: actor, Action: action, Target: auditTarget,
	}); err != nil {
		slogError(w, "retention settings: audit", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slogError(w, "retention settings: commit", err)
		return
	}
	slog.Info("retention policy changed", "action", action, "change", auditTarget, "actor", userIDFrom(ctx))
	s.writeRetentionView(w, r)
}

func (s *Server) writeRetentionView(w http.ResponseWriter, r *http.Request) {
	view, err := s.retentionView(r.Context())
	if err != nil {
		slogError(w, "retention settings", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) retentionConflict(w http.ResponseWriter, r *http.Request, msg string) {
	view, err := s.retentionView(r.Context())
	if err != nil {
		slog.Warn("retention settings: fresh view for a conflict failed", "error", err)
		view = nil
	}
	writeJSON(w, http.StatusConflict, retentionConflict{Error: msg, Current: view})
}
