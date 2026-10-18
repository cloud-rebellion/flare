package api

import (
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/bright-interaction/flare/internal/flarereport"
	"github.com/bright-interaction/flare/internal/metrics"
)

// Routes builds the full HTTP handler: middleware stack, JSON API, and the
// embedded SPA fallback.
func (s *Server) Routes(build fs.FS, csrfMW func(http.Handler) http.Handler) http.Handler {
	r := chi.NewRouter()
	httpMetrics := metrics.Default
	r.Use(chimw.RequestID)
	r.Use(httpMetrics.Middleware(func(req *http.Request) string {
		return chi.RouteContext(req.Context()).RoutePattern()
	}))
	// Trusted-proxy-aware client IP: only honor X-Forwarded-For from an internal
	// proxy peer, so a public client cannot spoof it to dodge the login lockout
	// or ingest rate limit. Replaces chimw.RealIP (unconditional header trust).
	r.Use(realIP)
	r.Use(chimw.Recoverer)
	// Capture panics to Flare (self-reporting), then re-panic for Recoverer's
	// 500. Must sit after Recoverer (inner) so it sees the panic first.
	r.Use(flarereport.FlareRecoverer)
	r.Use(securityHeaders(cspForBuild(build)))
	r.Use(s.sessions.LoadAndSave)
	// This endpoint is intentionally unauthenticated for Prometheus scrapers;
	// bind it to a private listener or protect it at the reverse proxy. It only
	// exposes bounded route/integration/ingest counters and never tenant data.
	r.Get("/metrics", httpMetrics.Handler().ServeHTTP)
	// Liveness is dependency-free; readiness includes bounded database,
	// analytics, retention, and worker-pool state. Keep the aliases outside the
	// authenticated /api router so load balancers and orchestrators can probe
	// them without a session.
	r.Get("/livez", s.handleLive)
	r.Get("/readyz", s.handleReady)
	r.Get("/healthz", s.handleReady)

	ccsrf := conditionalCSRF(csrfMW)

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", s.handleHealth)
		r.With(csrfMW).Get("/csrf", s.handleCSRFToken)
		// HubSpot's cross-site consent return cannot carry Flare's Strict
		// session cookie; the one-time Lax transaction cookie binds it instead.
		r.Get("/connectors/hubspot/callback", s.handleHubSpotOAuthCallback)

		// Ingest: authenticated by the DSN public key, not the session, so
		// these sit outside CSRF and requireAuth. Sentry-wire compatible.
		// Rate-limited per DSN key so a captured key cannot flood writes.
		r.Group(func(r chi.Router) {
			r.Use(s.rateLimitIngest)
			r.Post("/{projectID}/envelope/", s.handleEnvelope)
			r.Post("/{projectID}/envelope", s.handleEnvelope)
			r.Post("/{projectID}/store/", s.handleStore)
			r.Post("/{projectID}/store", s.handleStore)
			r.Post("/{projectID}/events", s.handleStore)
			r.Post("/{projectID}/logs", s.handleNativeLogs)
			r.Post("/{projectID}/metrics", s.handleNativeMetrics)
			r.Post("/{projectID}/connector-attempts", s.handleConnectorAttempt)
			// Cron/job check-in: DSN-authed like ingest. GET too, so a bare
			// `curl` from a cron works; ?status=error marks the run failed.
			r.Post("/{projectID}/checkins/{slug}", s.handleCheckin)
			r.Get("/{projectID}/checkins/{slug}", s.handleCheckin)
		})
		// Business assertions can drive operator alerts. Their confidential
		// project key and limiter are separate from the public browser DSN.
		r.With(s.rateLimitBusinessCheck).Post("/{projectID}/business-checks", s.handleBusinessCheck)

		r.Group(func(r chi.Router) {
			r.Use(ccsrf)

			r.Post("/auth/register", s.handleRegister)
			r.Post("/auth/login", s.handleLogin)
			r.Post("/auth/logout", s.handleLogout)
			r.Post("/auth/forgot-password", s.handleForgotPassword)
			r.Post("/auth/reset-password", s.handleResetPassword)
			r.Post("/auth/accept-invite", s.handleAcceptInvite)
			r.Get("/auth/me", s.handleMe)
			// SSO browser redirects (GET, so CSRF middleware passes them through).
			r.Get("/auth/oidc/start", s.handleOIDCStart)
			r.Get("/auth/oidc/callback", s.handleOIDCCallback)

			r.Group(func(r chi.Router) {
				r.Use(s.requireAuth)

				// MCP: one JSON-RPC endpoint for an AI operator (Claude). Same
				// auth as the REST API (session or org API key); write tools
				// self-gate to member+ inside the handler; per-org rate limited.
				// POST-only.
				r.With(s.rateLimitMCP).Handle("/mcp", s.mcpHandler())

				// Changing your own password is available to EVERY authenticated
				// role, including viewer. It is an account action, not a
				// workspace one, and gating it behind a role would leave the
				// least-privileged users with no way to rotate a credential.
				// The handler requires the current password, so a borrowed
				// session is not enough on its own.
				r.Post("/auth/password", s.handleChangePassword)

				// Reads: any authenticated role (viewer+).
				r.Get("/overview", s.handleOverview)
				r.Get("/projects", s.handleListProjects)
				r.Get("/connectors/hubspot/availability", s.handleHubSpotOAuthAvailability)
				r.Get("/projects/{id}", s.handleGetProject)
				r.Get("/projects/by-dsn/{dsnID}", s.handleGetProjectByDsnID)
				r.Get("/projects/{id}/issues", s.handleListIssues)
				r.Get("/projects/{id}/logs", s.handleSearchLogs)
				r.Get("/projects/{id}/traces", s.handleListTraces)
				r.Get("/projects/{id}/traces/{traceID}", s.handleGetTrace)
				r.Get("/projects/{id}/analytics/log-volume", s.handleLogVolume)
				r.Get("/projects/{id}/analytics/span-latency", s.handleSpanLatency)
				r.Get("/projects/{id}/analytics/service-level", s.handleServiceLevel)
				r.Get("/projects/{id}/metrics", s.handleListMetrics)
				r.Get("/projects/{id}/metrics/query", s.handleQueryMetric)
				r.Get("/projects/{id}/alert-rules", s.handleListAlertRules)
				r.Get("/projects/{id}/monitors", s.handleListMonitors)
				r.Get("/projects/{id}/artifacts", s.handleListSourceMaps)
				r.Get("/projects/{id}/artifact-bundles/{bundleID}", s.handleGetSourceMapBundle)
				r.Get("/projects/{id}/artifact-bundle-retirements/{retirementID}", s.handleGetSourceMapRetirement)
				r.Get("/projects/{id}/symbol-artifacts", s.handleListNativeSymbolArtifacts)
				r.Get("/projects/{id}/native-symbol-health", s.handleListNativeSymbolHealth)
				r.Get("/projects/{id}/releases", s.handleListReleases)
				r.Get("/projects/{id}/release-health", s.handleListReleaseHealth)
				r.Get("/projects/{id}/release-freshness", s.handleListReleaseFreshness)
				r.Get("/projects/{id}/symbolication-health", s.handleListReleaseSymbolicationHealth)
				r.Get("/projects/{id}/client-health", s.handleListClientHealth)
				r.Get("/projects/{id}/connector-health", s.handleListProjectConnectorHealth)
				r.Get("/projects/{id}/connector-sync-health", s.handleListProjectConnectorSyncHealth)
				r.Get("/projects/{id}/connector-quota-health", s.handleListProjectConnectorQuotaHealth)
				r.Get("/projects/{id}/business-health", s.handleListBusinessHealth)
				r.Get("/projects/{id}/ingest-receipts", s.handleListIngestReceipts)
				r.Get("/projects/{id}/sdk-discards", s.handleListSDKDiscards)
				r.Get("/projects/{id}/ai-receipts", s.handleListAIActionReceipts)
				r.Get("/projects/{id}/ai-receipts/{receiptID}", s.handleGetAIActionReceipt)
				r.Get("/projects/{id}/automation-receipts", s.handleListAutomationReceipts)
				r.Get("/projects/{id}/automation-verification", s.handleGetAutomationVerification)
				r.Get("/projects/{id}/automation-next", s.handleGetAutomationNext)
				r.Get("/usage", s.handleListUsage)
				r.Get("/issues/{id}", s.handleGetIssue)
				r.Get("/issues/{id}/events", s.handleListIssueEvents)
				r.Get("/channels", s.handleListChannels)
				r.Get("/connector-health", s.handleListConnectorHealth)
				r.Get("/connector-quota-health", s.handleListConnectorQuotaHealth)
				r.Get("/connector-sync-health", s.handleListConnectorSyncHealth)
				r.Get("/analytics-health", s.handleAnalyticsHealth)
				r.Get("/keys", s.handleListAPIKeys)
				r.Get("/members", s.handleListMembers)
				r.Get("/integrations/github", s.handleGetGithubConfig)

				// Writes: member+ (viewers are read-only). API keys act here.
				r.Group(func(r chi.Router) {
					r.Use(s.requireRole("member"))

					r.Post("/projects", s.handleCreateProject)
					r.Post("/projects/provision", s.handleProvisionProject)
					r.Post("/projects/{id}/alert-rules", s.handleCreateAlertRule)
					r.Delete("/projects/{id}/alert-rules/{ruleID}", s.handleDeleteAlertRule)
					r.Post("/projects/{id}/monitors", s.handleCreateMonitor)
					r.Patch("/monitors/{id}", s.handleUpdateMonitor)
					r.Delete("/monitors/{id}", s.handleDeleteMonitor)
					r.Post("/projects/{id}/artifacts", s.handleUploadSourceMap)
					r.Post("/projects/{id}/artifact-bundles", s.handleUploadSourceMapBundle)
					r.Delete("/projects/{id}/artifacts/{artifactID}", s.handleDeleteSourceMap)
					r.Post("/projects/{id}/symbol-artifacts", s.handleUploadNativeSymbolArtifact)
					r.Delete("/projects/{id}/symbol-artifacts/{artifactID}", s.handleDeleteNativeSymbolArtifact)
					r.Post("/projects/{id}/releases", s.handleCreateRelease)
					r.Patch("/issues/{id}", s.handleUpdateIssueStatus)
					r.Post("/issues/{id}/triage", s.handleTriageIssue)
					r.Post("/projects/{id}/automation-claims", s.handleCreateAutomationClaim)
					r.Post("/projects/{id}/automation-receipts", s.handleCreateAutomationReceipt)
					r.Post("/projects/{id}/automation-receipts/{receiptID}/verify", s.handleVerifyAutomationReceipt)
					r.Post("/channels", s.handleCreateChannel)
					r.Post("/channels/{id}/test", s.handleTestChannel)
					r.Patch("/channels/{id}", s.handleUpdateChannel)
					r.Delete("/channels/{id}", s.handleDeleteChannel)
					r.Post("/issues/{id}/github", s.handleCreateGithubIssue)
				})

				// Irreversible erasure and credential management: admin+, which
				// no API key can ever reach (apiKeyRole clamps a key to viewer or
				// member and refuses anything else).
				//
				// Giving keys their own role fixed the DEFAULT (new keys are
				// read-only) but left these three routes inside member, so a key
				// minted for CI provisioning or source-map upload still carried
				// DELETE /projects/{id}, which erases a project and all of its
				// telemetry in one transaction, plus the ability to mint and
				// revoke keys including its own successors. A leaked CI key is
				// the scenario the finding names, and the leak does not have to
				// be malicious: a build script with a wrong id deletes a project.
				//
				// These are interactive, human decisions. Provision, artifacts,
				// releases, alert rules, monitors and issue writes stay at member
				// so CI keeps working.
				r.Group(func(r chi.Router) {
					r.Use(s.requireRole("admin"))

					r.Delete("/projects/{id}", s.handleDeleteProject)
					r.Delete("/projects/{id}/artifact-bundles/{bundleID}", s.handleRetireSourceMapBundle)
					r.Post("/keys", s.handleCreateAPIKey)
					r.Delete("/keys/{id}", s.handleDeleteAPIKey)
					r.Get("/projects/{id}/business-keys", s.handleListBusinessCheckKeys)
					r.Post("/projects/{id}/business-keys", s.handleCreateBusinessCheckKey)
					r.Delete("/projects/{id}/business-keys/{keyID}", s.handleRevokeBusinessCheckKey)
				})

				// Team management + integration secrets: admin+.
				r.Group(func(r chi.Router) {
					r.Use(s.requireRole("admin"))

					r.Patch("/members/{userID}", s.handleUpdateMemberRole)
					r.Delete("/members/{userID}", s.handleRemoveMember)
					r.Get("/invites", s.handleListInvites)
					r.Post("/invites", s.handleInviteMember)
					r.Delete("/invites/{inviteID}", s.handleRevokeInvite)
					r.Put("/integrations/github", s.handleSetGithubConfig)
					r.Delete("/integrations/github", s.handleDeleteGithubConfig)
					r.Get("/audit-log", s.handleListAuditLog)
					r.Get("/export", s.handleExport)

					// Outstanding right-to-erasure obligations for this org (cold
					// tier not yet purged). An admin can see what still has to be
					// erased from the object store and mark one done once they have,
					// instead of hand-writing SQL against pending_erasures.
					r.Get("/erasures", s.handleListOpenErasures)
					r.Post("/erasures/{id}/complete", s.handleCompleteErasure)
					r.Get("/integrations/oidc", s.handleGetOIDCConfig)
					r.Get("/integrations/ai", s.handleGetAIConfig)
					r.Put("/integrations/ai", s.handleSetAIConfig)
					r.Delete("/integrations/ai", s.handleDeleteAIConfig)
					// Connector credentials are encrypted at rest and never returned.
					// Keep the whole registry behind admin: a member may observe
					// connector health, but only an administrator can alter provider
					// installation metadata or rotate a credential.
					r.Get("/connector-connections", s.handleListConnectorConnections)
					r.Get("/projects/{id}/stage-verifier", s.handleGetStageVerifierBinding)
					r.Put("/projects/{id}/stage-verifier", s.handleSetStageVerifierBinding)
					r.Delete("/projects/{id}/stage-verifier", s.handleDeleteStageVerifierBinding)
					r.Post("/connector-connections", s.handleCreateConnectorConnection)
					r.Post("/connectors/hubspot/start", s.handleHubSpotOAuthStart)
					r.Get("/connector-connections/{id}", s.handleGetConnectorConnection)
					r.Patch("/connector-connections/{id}", s.handleUpdateConnectorConnection)
					r.Delete("/connector-connections/{id}", s.handleDeleteConnectorConnection)
					r.Post("/connector-connections/{id}/check", s.handleCheckConnectorConnection)
					r.Post("/connector-connections/{id}/sync", s.handleRunConnectorSync)
				})

				// Owner only.
				r.Group(func(r chi.Router) {
					r.Use(s.requireRole("owner"))

					// Workspace erasure.
					r.Delete("/org", s.handleDeleteOrg)

					// WRITING the SSO config is an authentication-authority
					// change: whoever points the org at an IdP can have that IdP
					// assert any member's email. At admin+ that was a direct
					// admin -> owner escalation path. Reads stay at admin+.
					r.Put("/integrations/oidc", s.handleSetOIDCConfig)
					r.Delete("/integrations/oidc", s.handleDeleteOIDCConfig)
				})
			})
		})
	})

	// OTLP/HTTP ingest lives at the spec path (clients append /v1/logs to the
	// configured OTLP endpoint). DSN-key auth, no CSRF/session, same rate limit.
	r.With(s.rateLimitIngest).Post("/otlp/v1/logs", s.handleOTLPLogs)
	r.With(s.rateLimitIngest).Post("/otlp/v1/traces", s.handleOTLPTraces)
	r.With(s.rateLimitIngest).Post("/otlp/v1/metrics", s.handleOTLPMetrics)

	// Deep-link from a DSN (numeric dsn_id) to the dashboard project page.
	r.Get("/go/{dsnID}", s.handleDSNRedirect)

	r.Handle("/*", spaHandler(build))
	return r
}
