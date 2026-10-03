package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// securityProjectSlug is the per-org project Flare writes its own security
// signals into, so they surface in the dashboard, group, and alert like any
// other issue instead of vanishing into the container log.
const securityProjectSlug = "flare-security"

// recordSecurityEvent records one of Flare's own security signals (e.g. an
// ingest-auth rejection or a login lockout) as a grouped issue in the org's
// flare-security project. orgID may be empty for a system-level signal with no
// owning tenant (an ingest key that matched no project); it then falls back to
// the first/owner org. Fully fire-and-forget and throttled so a flood of
// rejected requests can never slow or overwhelm the real request path.
func (s *Server) recordSecurityEvent(orgID, kind, detail, ip string) {
	// Throttle BEFORE spawning: per (kind, ip) and a global per-kind cap. Both
	// must allow. Grouping already collapses same-kind events into one issue,
	// so throttling only bounds write volume, not detectability.
	if !s.secIPLimiter.Allow(kind+"|"+ip) || !s.secGlobalLimiter.Allow(kind) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		org := orgID
		if org == "" {
			org = s.systemOrgID(ctx)
		}
		if org == "" {
			return // no org registered yet (fresh install)
		}
		project, err := s.ensureSecurityProject(ctx, org)
		if err != nil || project == nil {
			slog.Warn("security event: ensure project failed", "kind", kind, "error", err)
			return
		}
		if _, err := s.ingestOne(ctx, project, buildSecurityEventJSON(kind, detail)); err != nil {
			slog.Warn("security event: record failed", "kind", kind, "error", err)
		}
	}()
}

// systemOrgID returns (and caches) the oldest org, used as the scope for
// security events that have no owning tenant.
func (s *Server) systemOrgID(ctx context.Context) string {
	s.secMu.Lock()
	cached := s.systemOrg
	s.secMu.Unlock()
	if cached != "" {
		return cached
	}
	org, err := s.q.GetFirstOrg(ctx)
	if err != nil {
		return ""
	}
	s.secMu.Lock()
	s.systemOrg = org.ID
	s.secMu.Unlock()
	return org.ID
}

// ensureSecurityProject get-or-creates the flare-security project for an org
// (cached), and on first creation seeds a spike alert rule so a burst of
// security events pages a human with no manual setup.
func (s *Server) ensureSecurityProject(ctx context.Context, org string) (*generated.Project, error) {
	s.secMu.Lock()
	if p := s.secProjects[org]; p != nil {
		s.secMu.Unlock()
		return p, nil
	}
	s.secMu.Unlock()

	if p, err := s.q.GetProjectBySlug(ctx, generated.GetProjectBySlugParams{OrgID: org, Slug: securityProjectSlug}); err == nil {
		s.cacheSecProject(org, p)
		return p, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	publicKey, err := id.Token(16)
	if err != nil {
		return nil, err
	}
	dsnID, err := id.Numeric(12)
	if err != nil {
		return nil, err
	}
	p, err := s.q.CreateProject(ctx, generated.CreateProjectParams{
		ID: id.New(), OrgID: org, Name: "Flare Security", Slug: securityProjectSlug,
		Platform: "flare", PublicKey: publicKey, DsnID: dsnID,
	})
	if err != nil {
		// Lost a race: fall back to the existing row.
		if p2, e2 := s.q.GetProjectBySlug(ctx, generated.GetProjectBySlugParams{OrgID: org, Slug: securityProjectSlug}); e2 == nil {
			s.cacheSecProject(org, p2)
			return p2, nil
		}
		return nil, err
	}
	// Seed alerts so security signals page without manual setup: a spike rule
	// for a burst (brute force, DSN abuse) and a new_issue rule so the FIRST
	// occurrence of a distinct signal (e.g. the first secret leak) alerts too.
	// Best-effort: a failure still leaves the project usable.
	_, _ = s.q.CreateAlertRule(ctx, generated.CreateAlertRuleParams{
		ID: id.New(), ProjectID: p.ID, OrgID: org,
		Name: "Security event spike", Type: "spike", Threshold: 10, WindowMinutes: 5, MetricLabels: json.RawMessage(`{}`), Enabled: true,
	})
	_, _ = s.q.CreateAlertRule(ctx, generated.CreateAlertRuleParams{
		ID: id.New(), ProjectID: p.ID, OrgID: org,
		Name: "New security event", Type: "new_issue", MetricLabels: json.RawMessage(`{}`), Enabled: true,
	})
	s.cacheSecProject(org, p)
	return p, nil
}

func (s *Server) cacheSecProject(org string, p *generated.Project) {
	s.secMu.Lock()
	s.secProjects[org] = p
	s.secMu.Unlock()
}

// invalidateSecCaches drops the cached flare-security project (and the system
// org, when that org is the one going away). The caches never expired, so
// deleting the flare-security project left a stale pointer that every later
// security event wrote against, silently disabling the org's security-signal
// pillar until the process restarted.
func (s *Server) invalidateSecCaches(org string) {
	s.secMu.Lock()
	delete(s.secProjects, org)
	if s.systemOrg == org {
		s.systemOrg = ""
	}
	s.secMu.Unlock()
}

// buildSecurityEventJSON renders a Sentry-shape event whose exception type is
// the constant "SecurityEvent" and whose value is the kind, so events group by
// kind (one issue per kind, count rising) via the existing fingerprint.
func buildSecurityEventJSON(kind, detail string) []byte {
	b, _ := json.Marshal(map[string]any{
		"level":    "warning",
		"platform": "flare",
		"message":  detail,
		"exception": map[string]any{
			"values": []map[string]any{{"type": "SecurityEvent", "value": kind}},
		},
	})
	return b
}
