// Package api wires Flare's HTTP surface: auth, projects, ingest, and the
// embedded SvelteKit dashboard.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/ai"
	"github.com/bright-interaction/flare/internal/alerts"
	"github.com/bright-interaction/flare/internal/analytics"
	"github.com/bright-interaction/flare/internal/config"
	"github.com/bright-interaction/flare/internal/connectorcheck"
	"github.com/bright-interaction/flare/internal/connectorsync"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/email"
	flaregithub "github.com/bright-interaction/flare/internal/github"
	"github.com/bright-interaction/flare/internal/hubspot"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/metrics"
	flareopenai "github.com/bright-interaction/flare/internal/openai"
	"github.com/bright-interaction/flare/internal/ratelimit"
	"github.com/bright-interaction/flare/internal/secretbox"
	"github.com/bright-interaction/flare/internal/slack"
	"github.com/bright-interaction/flare/internal/sourcemaps"
	"github.com/bright-interaction/flare/internal/telemetry"
	"github.com/bright-interaction/flare/internal/telemetry/pgstore"
)

const (
	// loginFailBudget / loginFailWindow lock an account+IP out after this many
	// failed logins, per the repo security rules (5 fails, 15-min cooldown).
	loginFailBudget = 5
	loginFailWindow = 15 * time.Minute
)

type Server struct {
	q                  *generated.Queries
	pool               *pgxpool.Pool
	store              telemetry.Store
	analytics          *analytics.Manager // may be nil when DuckDB failed to open
	sessions           *scs.SessionManager
	cfg                config.Config
	dispatcher         *alerts.Dispatcher
	mailer             *email.Mailer
	symbolicator       *sourcemaps.Resolver
	ai                 *ai.Client
	secrets            *secretbox.Cipher // encrypts integration secrets at rest
	hubspotProbe       connectorProbe
	connectorProbes    connectorcheck.Registry
	hubspotSync        *connectorsync.HubSpotWorker
	connectorScheduler *connectorsync.Scheduler

	// connectorReceiptMu guards the process-wide durability projection exposed
	// by /readyz. It contains only write counts and timestamps, never tenant or
	// provider data. A provider result is not retried when this boundary fails;
	// the projection makes that durability gap explicit to operators.
	connectorReceiptMu         sync.RWMutex
	connectorReceiptConfigured bool
	connectorReceiptStarted    uint64
	connectorReceiptCompleted  uint64
	connectorReceiptSucceeded  uint64
	connectorReceiptFailed     uint64
	connectorReceiptState      string
	connectorReceiptLastOK     time.Time
	connectorReceiptLastError  time.Time

	// loginLimiter locks out brute-force logins; ingestLimiter caps per-DSN-key
	// (or per-IP) ingest flooding; mcpLimiter caps per-org MCP request rate so a
	// member key cannot loop an expensive tool. All in-memory, no Redis.
	loginLimiter  *ratelimit.Limiter
	ingestLimiter *ratelimit.Limiter
	mcpLimiter    *ratelimit.Limiter
	// resetLimiter caps password-reset requests per email+IP so /forgot-password
	// cannot be used for account enumeration or reset-email bombing.
	resetLimiter *ratelimit.Limiter
	// resetConsumeLimiter caps the OTHER half of the reset flow, per IP.
	// /forgot-password only MINTS a link; /auth/reset-password is the step that
	// spends it, and it is the one that runs bcrypt. Limiting the mint and not
	// the spend leaves the expensive half open, which is exactly how it shipped.
	resetConsumeLimiter *ratelimit.Limiter

	// signupLimiter caps registration attempts per IP so the one unauthenticated
	// route that creates rows cannot be looped.
	signupLimiter *ratelimit.Limiter
	// inviteLimiter caps invite acceptances per IP. /auth/accept-invite is the
	// OTHER unauthenticated POST, and once register was gated it was the last one
	// with no ceiling at all: a bcrypt at cost 12 plus two queries, reachable
	// without an account.
	inviteLimiter *ratelimit.Limiter
	// testLimiter caps per-org "send test notification" calls so the test route
	// cannot be looped to spam a configured recipient or probe public hosts.
	testLimiter *ratelimit.Limiter
	// monitorAlertLimiter caps monitor-failure alerts PER ORG, independent of
	// the per-monitor cap.
	//
	// The per-monitor key includes the slug, which the caller chooses, and
	// UpsertMonitorCheckin CREATES the monitor if it does not exist. So a DSN
	// public key could invent an unlimited number of slugs, each auto-registering
	// and each getting its own fresh per-monitor budget. Capping per (org, slug)
	// bounds an honestly-flapping cron and bounds nothing at all against someone
	// varying the slug.
	monitorAlertLimiter *ratelimit.Limiter
	// issueAlertLimiter caps issue alerts PER ORG, the same way
	// monitorAlertLimiter caps monitor alerts.
	//
	// The monitor path spells out this exact attack and caps it twice; the
	// new-issue path had no equivalent at all. bgAlertWorkers is a CONCURRENCY
	// bound, not a volume bound: it holds parallel sends at 24 and drops the
	// overflow, so a sustained flood of unique fingerprints from one DSN key
	// produced a sustained 24-way email/Slack/webhook send with nothing
	// throttling total volume, into the org's own recipients.
	issueAlertLimiter *ratelimit.Limiter

	// Security-event recording: Flare's own security signals (ingest-auth
	// rejections, login lockouts) become grouped issues in a per-org
	// "flare-security" project. secIPLimiter throttles per (kind, ip),
	// secGlobalLimiter caps total volume per kind, so a flood of rejected
	// requests cannot self-DoS the write path. secMu guards the caches.
	secMu            sync.Mutex
	systemOrg        string
	secProjects      map[string]*generated.Project
	secIPLimiter     *ratelimit.Limiter
	secGlobalLimiter *ratelimit.Limiter

	// bgSlots bounds the detached goroutines ingest spawns per event. Ingest used
	// to fire one unbounded `go func` per event, so a single large envelope could
	// create tens of thousands of goroutines all queueing on a ~20-connection pgx
	// pool and starving every other tenant. Acquire is non-blocking: when the
	// pool is saturated the work is DROPPED rather than queued, because ingest
	// must never block and a backlog of stale alert evaluations has no value.
	//
	// Alerting and AI triage get SEPARATE pools on purpose. Sharing one meant a
	// burst of new fingerprints could fill every slot with 90-second BYOAI
	// completions, so 15-second alert evaluation, the thing that actually pages a
	// human, was dropped while the optional nice-to-have ran.
	bgSlots map[string]chan struct{}
	// allowPrivateAI is the EFFECTIVE value of FLARE_ALLOW_PRIVATE_AI_ENDPOINT
	// after the multi-workspace check in NewServer. Read this, never cfg: the
	// config value is what the operator asked for, this is what applies.
	allowPrivateAI bool

	bgSlotsOnce sync.Once
	// orgSlots counts the background slots each org currently holds, per job
	// class, so no single tenant can occupy the shared pool.
	orgSlotMu sync.Mutex
	orgSlots  map[string]int
	// bgWG tracks in-flight background jobs so shutdown can wait for them.
	// Without it SIGTERM dropped alert dispatches that were mid-flight and
	// abandoned AI triage calls whose token budget had already been claimed.
	bgWG sync.WaitGroup
}

// WaitBackground blocks until every in-flight background job finishes or the
// context expires. Called during graceful shutdown, after the HTTP listener has
// stopped accepting, so a page that was already being dispatched still goes out.
func (s *Server) WaitBackground(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		s.bgWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("shutdown: background jobs still running, giving up on them")
	}
}

// Background pool ceilings, sized against the ~20-connection pgx pool.
const (
	bgAlertWorkers  = 24
	bgTriageWorkers = 8
	// bgPerOrgWorkers is the share of a class's slots any ONE tenant may hold.
	//
	// A global pool with no per-tenant accounting is a pool one tenant can own.
	// Two webhook channels pointing at a host that accepts the TCP connection
	// and never answers, plus a flood of distinct fingerprints to that org's
	// own DSN (1200/min is allowed), occupied every alert-eval slot
	// continuously; goBackground then DROPS the overflow with a log line, no
	// retry and no queue, so every OTHER tenant's new-issue, regression, spike,
	// monitor-failed and watchdog alert silently stopped. That is the product's
	// core promise failing under one tenant's control, and one org with a
	// genuinely dead endpoint does it by accident.
	bgPerOrgWorkers = 6
)

// bgPool returns the worker pool for a job class, lazily initialised so a Server
// built directly in a test (not via NewServer) still works instead of blocking
// forever on a nil channel.
func (s *Server) bgPool(name string) chan struct{} {
	s.bgSlotsOnce.Do(func() {
		if s.bgSlots == nil {
			s.bgSlots = map[string]chan struct{}{}
		}
		if s.bgSlots["auto-triage"] == nil {
			s.bgSlots["auto-triage"] = make(chan struct{}, bgTriageWorkers)
		}
		if s.bgSlots["default"] == nil {
			s.bgSlots["default"] = make(chan struct{}, bgAlertWorkers)
		}
	})
	if ch, ok := s.bgSlots[name]; ok {
		return ch
	}
	return s.bgSlots["default"]
}

// goBackground runs fn on a bounded worker slot with its own detached, deadlined
// context. Returns false (and drops fn) when every slot for that class is busy.
//
// Prefer goBackgroundFor for anything a tenant can trigger.
func (s *Server) goBackground(name string, timeout time.Duration, fn func(context.Context)) bool {
	return s.goBackgroundFor("", name, timeout, fn)
}

// goBackgroundFor is goBackground with per-org accounting on top of the global
// pool: one tenant may hold at most bgPerOrgWorkers slots of a class, so a
// tenant whose delivery endpoints hang cannot starve every other tenant's
// alerts. An empty org means server-owned work with no tenant to attribute it
// to and takes the global pool only.
func (s *Server) goBackgroundFor(org, name string, timeout time.Duration, fn func(context.Context)) bool {
	pool := s.bgPool(name)
	if org != "" && !s.claimOrgSlot(org, name) {
		slog.Warn("background work dropped: org is at its share of the pool",
			"job", name, "org_id", org, "per_org_capacity", bgPerOrgWorkers)
		return false
	}
	select {
	case pool <- struct{}{}:
	default:
		if org != "" {
			s.releaseOrgSlot(org, name)
		}
		slog.Warn("background work dropped: all worker slots busy", "job", name, "capacity", cap(pool))
		return false
	}
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		defer func() { <-pool }()
		if org != "" {
			defer s.releaseOrgSlot(org, name)
		}
		defer func() {
			if r := recover(); r != nil {
				slog.Error("background job panicked", "job", name, "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		fn(ctx)
	}()
	return true
}

// claimOrgSlot takes one of an org's per-class slots, or reports that it has
// none left. Counters are deleted at zero so the map cannot grow with the
// tenant list over the process lifetime.
func (s *Server) claimOrgSlot(org, class string) bool {
	key := class + "\x00" + org
	s.orgSlotMu.Lock()
	defer s.orgSlotMu.Unlock()
	if s.orgSlots == nil {
		s.orgSlots = map[string]int{}
	}
	if s.orgSlots[key] >= bgPerOrgWorkers {
		return false
	}
	s.orgSlots[key]++
	return true
}

func (s *Server) releaseOrgSlot(org, class string) {
	key := class + "\x00" + org
	s.orgSlotMu.Lock()
	defer s.orgSlotMu.Unlock()
	if s.orgSlots[key] <= 1 {
		delete(s.orgSlots, key)
		return
	}
	s.orgSlots[key]--
}

func NewServer(pool *pgxpool.Pool, sessions *scs.SessionManager, cfg config.Config, analyticsMgr *analytics.Manager) *Server {
	mailer := email.New(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUser, cfg.SMTPPass, cfg.SMTPFrom, cfg.SMTPFromName, cfg.SMTPTLS)

	// FLARE_ALLOW_PRIVATE_AI_ENDPOINT exists so a developer can point BYOAI at
	// a local Ollama. On a SHARED deployment it is a global kill switch on a
	// per-tenant control: with it set, ANY org can point base_url at an
	// internal address and read the response back through triage. So it only
	// applies while the instance is what it was written for, one workspace.
	//
	// Fails closed by ignoring the flag rather than refusing to boot: an
	// operator who added a second workspace to a dev instance should lose the
	// escape hatch, not lose the server.
	allowPrivateAI := cfg.AllowPrivateAIEndpoint
	if allowPrivateAI {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		n, err := generated.New(pool).CountOrgs(ctx)
		cancel()
		if err != nil {
			slog.Warn("could not count workspaces; ignoring FLARE_ALLOW_PRIVATE_AI_ENDPOINT", "error", err)
			allowPrivateAI = false
		} else if n > 1 {
			slog.Error("FLARE_ALLOW_PRIVATE_AI_ENDPOINT is set on an instance with more than one "+
				"workspace, which would let any tenant reach internal addresses through AI triage. "+
				"Ignoring it; the SSRF guard stays on.", "workspaces", n)
			allowPrivateAI = false
		}
	}
	queries := generated.New(pool)
	secretCipher := secretbox.New(cfg.SecretKey)
	hubspotClient := hubspot.New()
	connectorProbes := newConnectorProbeRegistry(hubspotClient)
	hubspotWorker := connectorsync.NewHubSpotWorker(queries, secretCipher, hubspotClient)
	srv := &Server{
		q:                          queries,
		pool:                       pool,
		store:                      pgstore.New(pool),
		analytics:                  analyticsMgr,
		sessions:                   sessions,
		cfg:                        cfg,
		dispatcher:                 alerts.NewDispatcher(mailer),
		mailer:                     mailer,
		symbolicator:               sourcemaps.NewResolver(),
		ai:                         ai.New(!allowPrivateAI),
		allowPrivateAI:             allowPrivateAI,
		secrets:                    secretCipher,
		hubspotProbe:               hubspotClient,
		connectorProbes:            connectorProbes,
		hubspotSync:                hubspotWorker,
		connectorScheduler:         connectorsync.NewScheduler(queries, hubspotWorker),
		connectorReceiptConfigured: pool != nil,
		connectorReceiptState:      "unobserved",

		loginLimiter:  ratelimit.New(loginFailBudget, loginFailWindow),
		ingestLimiter: ratelimit.New(cfg.IngestRatePerMin, time.Minute),
		mcpLimiter:    ratelimit.New(mcpRatePerMin, time.Minute),
		resetLimiter:  ratelimit.New(5, 15*time.Minute), // <=5 reset requests per (email, ip) / 15m
		// <=10 reset COMPLETIONS per IP / 15m, keyed on IP alone. resetLimiter
		// above covers /forgot-password and cannot cover this route: its key
		// folds in the email address, and this request carries no address, only
		// an opaque token, which must stay out of the key for the same reason
		// inviteLimiter keeps its own token out (a caller-chosen component is a
		// fresh budget, not a ceiling).
		//
		// A real reset costs ONE request. The two retries a person actually
		// makes, a password under 8 characters and a mismatched confirmation,
		// are both caught in the browser (minlength={8} and a password !==
		// confirm check in reset-password/+page.svelte), so neither reaches
		// here. Ten rather than resetLimiter's five because this bucket has no
		// email in it to spread a shared address across: a whole office behind
		// one NAT lands in a single bucket, and 15 minutes matches the token's
		// own life so a wrongly-locked address is never stuck past the link it
		// is trying to use.
		resetConsumeLimiter: ratelimit.New(10, 15*time.Minute),
		// <=5 registration attempts per IP / hour. Login and password reset were
		// both limited; register, the only unauthenticated route that WRITES two
		// rows, was not. Keyed on IP alone because the bootstrap gate below makes
		// the email irrelevant to the outcome once an install has a user.
		signupLimiter: ratelimit.New(5, time.Hour),
		// <=10 invite acceptances per IP / 15m. Wider than register's 5/hour and
		// on a shorter window, because the two routes have different shapes:
		// registration happens once per install, while invites arrive in batches
		// and a whole team can accept from one office NAT inside one window.
		//
		// A legitimate accept costs ONE request. The two retries a real person
		// makes, a password under 8 characters and a confirmation that does not
		// match, are both caught in the browser (minlength and an equality check
		// in accept-invite/+page.svelte), so neither reaches here. What does
		// reach here is a stale link, a network retry, and anyone driving the
		// API directly. So ten leaves room for several colleagues behind one
		// address and no room at all for a loop, and a wrongly-locked office is
		// back in fifteen minutes rather than an hour. What it buys: an address
		// can force at most 40 bcrypts an hour instead of as many as it can open
		// sockets for.
		inviteLimiter: ratelimit.New(10, 15*time.Minute),
		testLimiter:   ratelimit.New(10, time.Minute), // <=10 test-sends per org / min
		// <=20 monitor-failure alerts per ORG per minute, whatever the slug.
		// Above any real estate (a flapping fleet transitions a handful of
		// monitors a minute) and far below what a mailbox tolerates.
		monitorAlertLimiter: ratelimit.New(20, time.Minute),
		// <=30 issue alerts per ORG per minute. Above any real incident (an
		// outage produces a handful of distinct fingerprints a minute, and
		// TrySetIssueSpike already dedups per issue) and far below what a
		// mailbox or a Slack channel tolerates.
		issueAlertLimiter: ratelimit.New(30, time.Minute),

		secProjects:      map[string]*generated.Project{},
		secIPLimiter:     ratelimit.New(1, 10*time.Second), // <=1 per (kind, ip) / 10s
		secGlobalLimiter: ratelimit.New(60, time.Minute),   // <=60 per kind / min

		bgSlots: map[string]chan struct{}{
			"default":     make(chan struct{}, bgAlertWorkers),
			"auto-triage": make(chan struct{}, bgTriageWorkers),
		},
	}
	// Persist each delivery outcome so a silently-failing channel becomes
	// visible (last_ok_at / last_error) instead of vanishing into a log line.
	srv.dispatcher.Recorder = srv.recordChannelDelivery
	// Persist bounded, payload-free connector attempts for authenticated health
	// reads. Unscoped calls are rejected by integration.RecordResult and the
	// compatibility Record wrapper continues to drop them without changing
	// provider-call semantics.
	integration.SetRecorderWithError(srv.recordConnectorAttempt)
	return srv
}

// newConnectorProbeRegistry is the compile-time allowlist for on-demand
// connector checks. Sync remains deliberately separate and HubSpot-only.
func newConnectorProbeRegistry(hubspotProbe connectorProbe) connectorcheck.Registry {
	if hubspotProbe == nil {
		hubspotProbe = hubspot.New()
	}
	return connectorcheck.NewRegistry(map[string]connectorcheck.Adapter{
		"hubspot": {Operation: "account_details", Probe: hubspotProbe},
		"github":  {Operation: "user", Probe: flaregithub.NewHealthClient()},
		"slack":   {Operation: "auth.test", Probe: slack.New()},
		"openai":  {Operation: "models", Probe: flareopenai.New()},
	})
}

// dispatchToProject sends one notification to the channels routed to a project,
// which is every channel routed to it PLUS every channel with no routing at all.
//
// This is the ONLY dispatch helper. dispatchToOrg used to sit beside it with
// zero callers and a doc comment admitting it "fans out to EVERY enabled
// channel in the org, ignoring routing", which is a routing bypass waiting for
// someone to reach for the shorter name. Org-wide fan-out is what put a
// production incident and a side project in the same Slack; it is deleted, not
// deprecated.
func (s *Server) dispatchToProject(ctx context.Context, org, project string, n alerts.Notification) {
	if n.ProjectID == "" {
		n.ProjectID = project
	}
	chans, err := s.q.ListEnabledChannelsForProject(ctx, generated.ListEnabledChannelsForProjectParams{
		OrgID: org, ProjectID: project,
	})
	if err != nil || len(chans) == 0 {
		return
	}
	channels := make([]alerts.Channel, 0, len(chans))
	for _, c := range chans {
		channels = append(channels, alerts.Channel{ID: c.ID, OrgID: c.OrgID, Type: c.Type, Config: s.decryptChannelConfig(c.Type, c.Config)})
	}
	s.dispatcher.Dispatch(ctx, channels, n)
}
func (s *Server) recordChannelDelivery(ctx context.Context, orgID, channelID string, derr error) {
	// Detach from the dispatch context, which a prior slow/hung channel in the
	// same org may already have cancelled, with a fresh short deadline. Without
	// this, the exact outcome the feature exists to record - a delivery timeout
	// - could itself fail to persist ("context canceled").
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	errMsg := pgtype.Text{}
	ok := derr == nil
	if !ok {
		msg := derr.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		errMsg = pgtype.Text{String: msg, Valid: true}
	}
	if err := s.q.RecordChannelDelivery(ctx, generated.RecordChannelDeliveryParams{
		AttemptedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		Ok:          ok,
		ErrorMsg:    errMsg,
		ID:          channelID,
		OrgID:       orgID,
	}); err != nil {
		slog.Warn("record channel delivery", "channel_id", channelID, "error", err)
	}
}

func (s *Server) recordConnectorAttempt(ctx context.Context, a integration.Attempt) error {
	metrics.RecordConnectorReceipt("start", "started")
	s.connectorReceiptMu.Lock()
	s.connectorReceiptStarted++
	s.connectorReceiptMu.Unlock()
	if s.q == nil {
		err := errors.New("connector receipt store is not configured")
		metrics.RecordConnectorReceipt("complete", "error")
		s.observeConnectorReceipt(false)
		slog.Warn("record connector attempt", "provider", a.Provider, "operation", a.Operation, "error", err)
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	projectID := pgtype.Text{}
	if a.Scope.ProjectID != "" {
		projectID = pgtype.Text{String: a.Scope.ProjectID, Valid: true}
	}
	connectionID := pgtype.Text{}
	if a.Scope.ConnectionID != "" {
		connectionID = pgtype.Text{String: a.Scope.ConnectionID, Valid: true}
	}
	if err := s.q.InsertConnectorAttempt(ctx, generated.InsertConnectorAttemptParams{
		ID:             id.New(),
		OrgID:          a.Scope.OrgID,
		ProjectID:      projectID,
		ConnectionID:   connectionID,
		Provider:       a.Provider,
		Operation:      a.Operation,
		Channel:        a.Channel,
		Outcome:        a.Outcome,
		StatusClass:    a.StatusClass,
		ErrorCode:      a.ErrorCode,
		ElapsedMs:      a.Elapsed.Milliseconds(),
		QuotaRemaining: optionalInt8(a.QuotaRemaining),
		RetryAfterMs:   optionalInt8(a.RetryAfterMS),
		SyncLagMs:      optionalInt8(a.SyncLagMS),
		AttemptedAt:    pgtype.Timestamptz{Time: a.AttemptedAt.UTC(), Valid: true},
		CompletedAt:    pgtype.Timestamptz{Time: a.CompletedAt.UTC(), Valid: true},
	}); err != nil {
		metrics.RecordConnectorReceipt("complete", "error")
		s.observeConnectorReceipt(false)
		slog.Warn("record connector attempt", "provider", a.Provider, "operation", a.Operation, "error", err)
		// Do not let driver text, SQL fragments, or deployment details cross the
		// connector boundary. Provider adapters reduce this to the fixed
		// `unavailable` receipt status and keep their own result unchanged.
		return errors.New("connector receipt persistence failed")
	}
	metrics.RecordConnectorReceipt("complete", "ok")
	s.observeConnectorReceipt(true)
	return nil
}

// dsn builds the Sentry-style DSN a client SDK uses to point at this project.
// Shape: {scheme}://{publicKey}@{host}/{dsnID}. dsnID is numeric because
// @sentry/* SDKs reject a non-numeric project id in the DSN path.
func (s *Server) dsn(publicKey, dsnID string) string {
	scheme, host := splitBaseURL(s.cfg.BaseURL)
	return fmt.Sprintf("%s://%s@%s/%s", scheme, publicKey, host, dsnID)
}

// otlpEndpoint is the OTLP/HTTP base a collector or SDK exports logs+traces to.
func (s *Server) otlpEndpoint() string {
	return strings.TrimRight(s.cfg.BaseURL, "/") + "/otlp"
}

func splitBaseURL(base string) (scheme, host string) {
	scheme = "https"
	host = base
	if i := strings.Index(base, "://"); i >= 0 {
		scheme = base[:i]
		host = base[i+3:]
	}
	host = strings.TrimRight(host, "/")
	return scheme, host
}
