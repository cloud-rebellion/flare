package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugStrip.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	OrgName  string `json:"org_name"`
}

type userResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	OrgID string `json:"org_id"`
	Role  string `json:"role"`
}

func toUserResponse(u *generated.User) userResponse {
	return userResponse{ID: u.ID, Email: u.Email, OrgID: u.OrgID, Role: u.Role}
}

// handleRegister creates a new org plus its first (owner) user and logs them in.
//
// This is the self-host FIRST-RUN path, and it is now gated as one. The doc
// comment said "further members are invited" while the code let anyone on the
// internet mint an unlimited number of orgs, on every deployment, with no way to
// turn it off: CountUsers was written for this check, documented as the "pre-auth
// bootstrap check (is this a fresh install)", and never wired to a caller. Each
// minted org carries its own INGEST_RATE_PER_MIN budget, so ingest capacity
// scaled linearly with orgs created, against one shared Postgres.
//
// Three things had to move together, because they are one route's failure:
//
//   - The bootstrap gate. Open only while the install has no users, or when
//     FLARE_ALLOW_SIGNUP is explicitly set.
//   - The rate limit. Login and password reset were both limited; the only
//     unauthenticated route that WRITES was not.
//   - The transaction. CreateOrg and CreateUser ran as two statements, so two
//     concurrent registrations for one email left an orphan org with zero users:
//     unreachable through the API (DELETE /org needs owner auth from inside it),
//     and if it happened to be the oldest org, systemOrgID picks it via
//     GetFirstOrg and writes every tenantless security event into a workspace
//     nobody can open. provisionSSOUser had already learned this and wrapped its
//     own two steps; register is its twin and did not.
//
// The closed answer is deliberately identical for a taken and a free address.
// handleLogin and handleForgotPassword are both hardened against enumeration and
// this route undid it with a 409-versus-201 oracle on every address in the world.
// With the gate closed, which is every configuration except a fresh install and
// an explicit opt-in, no response here varies by email.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	// Before the body is parsed: the limit must not depend on well-formed input.
	if !s.signupLimiter.Allow("signup:" + clientIP(r)) {
		w.Header().Set("Retry-After", "3600")
		writeErr(w, http.StatusTooManyRequests, "too many registration attempts, try again later")
		return
	}

	var req registerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if !strings.Contains(req.Email, "@") || len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "valid email and password (min 8 chars) required")
		return
	}
	orgName := strings.TrimSpace(req.OrgName)
	if orgName == "" {
		orgName = req.Email
	}

	ctx := r.Context()

	// The bootstrap gate, before any lookup that could vary by email.
	if !s.cfg.AllowSignup {
		n, err := s.q.CountUsers(ctx)
		if err != nil {
			slogError(w, "count users", err)
			return
		}
		if n > 0 {
			writeErr(w, http.StatusForbidden, "registration is closed on this instance")
			return
		}
	}

	if _, err := s.q.GetUserByEmail(ctx, req.Email); err == nil {
		writeErr(w, http.StatusConflict, "email already registered")
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slogError(w, "lookup user", err)
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		slogError(w, "hash password", err)
		return
	}

	slug := slugify(orgName)
	if slug == "" {
		slug = "org"
	}
	slug = slug + "-" + id.New()[:6]

	org, user, err := s.createOrgWithOwner(ctx, orgName, slug, req.Email, hash)
	if err != nil {
		slogError(w, "create org and owner", err)
		return
	}

	s.establishSession(ctx, user.ID, org.ID)
	writeJSON(w, http.StatusCreated, toUserResponse(user))
}

// createOrgWithOwner writes the org and its first owner in ONE transaction, so a
// failed second step cannot leave an org behind with no user able to reach it.
// Same reasoning, and the same shape, as provisionSSOUser.
func (s *Server) createOrgWithOwner(ctx context.Context, orgName, slug, email, passwordHash string) (*generated.Org, *generated.User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	q := s.q.WithTx(tx)
	org, err := q.CreateOrg(ctx, generated.CreateOrgParams{ID: id.New(), Name: orgName, Slug: slug})
	if err != nil {
		return nil, nil, err
	}
	user, err := q.CreateUser(ctx, generated.CreateUserParams{
		ID: id.New(), OrgID: org.ID, Email: email, PasswordHash: passwordHash, Role: "owner",
	})
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return org, user, nil
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))

	// Brute-force lockout: 5 failed attempts per email+IP within 15 minutes.
	lockKey := "login:" + limiterEmailKey(req.Email) + "|" + clientIP(r)
	if s.loginLimiter.Blocked(lockKey) {
		w.Header().Set("Retry-After", "900")
		writeErr(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}

	ctx := r.Context()
	user, err := s.q.GetUserByEmail(ctx, req.Email)
	// Always run a bcrypt comparison (against a dummy hash when the email is
	// unknown) so response time does not reveal whether the account exists.
	storedHash := ""
	if err == nil {
		storedHash = user.PasswordHash
	}
	if err != nil || !auth.VerifyPasswordConstantTime(storedHash, req.Password) {
		s.loginLimiter.Record(lockKey)
		// Once the failures cross the lockout threshold, that is a brute-force
		// signal worth surfacing (the throttle in recordSecurityEvent keeps a
		// sustained attack from flooding).
		if s.loginLimiter.Blocked(lockKey) {
			// Record against the TARGETED user's org, never the fallback
			// system org. An empty org resolves to GetFirstOrg (the oldest
			// workspace), so every tenant's lockouts were written into one
			// unrelated org's security project, leaking that user's email and
			// IP across the tenant boundary.
			//
			// When the email does not resolve to an account there is no org to
			// attribute it to, and req.Email is then unauthenticated
			// attacker-controlled text, so it must not be turned into an issue
			// title in anyone's workspace. Log it and stop.
			if err == nil {
				s.recordSecurityEvent(user.OrgID, "login-lockout",
					"repeated failed logins for "+req.Email+" from "+clientIP(r), clientIP(r))
			} else {
				slog.Warn("login lockout for unknown account", "ip", clientIP(r))
			}
		}
		// Same message either way: never reveal which half failed.
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	s.loginLimiter.Reset(lockKey) // clear the counter on success
	s.establishSession(ctx, user.ID, user.OrgID)
	writeJSON(w, http.StatusOK, toUserResponse(user))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// A HubSpot consent started in another tab has a Lax callback cookie even
	// after the Strict session ends. Revoke pending server-side state as well as
	// the browser cookie so a same-browser callback cannot finish after logout.
	s.clearHubSpotTx(w)
	if uid, org := s.sessions.GetString(r.Context(), "user_id"), s.sessions.GetString(r.Context(), "org_id"); uid != "" && org != "" && s.pool != nil {
		if _, err := s.pool.Exec(r.Context(), `DELETE FROM hubspot_oauth_transactions
			WHERE user_id=$1 AND org_id=$2 AND consumed_at IS NULL`, uid, org); err != nil {
			writeErr(w, http.StatusServiceUnavailable, "logout could not invalidate pending connections")
			return
		}
	}
	if err := s.sessions.Destroy(r.Context()); err != nil {
		slogError(w, "destroy session", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := s.sessions.GetString(ctx, "user_id")
	if uid == "" {
		writeErr(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	user, err := s.q.GetUserByID(ctx, uid)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	// This route sits OUTSIDE requireAuth, so it must repeat requireAuth's
	// revocation check itself. Without it, a session predating a password reset
	// still resolved here, so the SPA kept rendering as signed in (name, role,
	// org) for an attacker whose session was supposed to be dead.
	epoch := s.sessions.GetInt64(ctx, "auth_epoch")
	if epoch > 0 && user.SessionsValidFrom.Valid && user.SessionsValidFrom.Time.Unix() > epoch {
		_ = s.sessions.Destroy(ctx)
		writeErr(w, http.StatusUnauthorized, "session expired, please sign in again")
		return
	}
	writeJSON(w, http.StatusOK, toUserResponse(user))
}

// handleForgotPassword always responds 200 (never reveals whether the email
// exists). When the account exists and SMTP is configured, it mails a
// single-use, 1-hour reset link. Only the token hash is stored.
func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ok := func() { writeJSON(w, http.StatusOK, map[string]string{"status": "ok"}) }
	email := strings.ToLower(strings.TrimSpace(req.Email))

	// Rate-limit per email+IP. The limit trips on the key regardless of whether
	// the account exists, so it enables neither enumeration nor reset-email
	// bombing while still letting a genuine user retry a few times.
	if !s.resetLimiter.Allow("reset:" + limiterEmailKey(email) + "|" + clientIP(r)) {
		w.Header().Set("Retry-After", "900")
		writeErr(w, http.StatusTooManyRequests, "too many reset requests, try again later")
		return
	}

	ctx := r.Context()
	user, err := s.q.GetUserByEmail(ctx, email)
	if err != nil {
		ok() // unknown address: respond identically, send nothing.
		return
	}
	if !s.mailer.Enabled() {
		// Say so, rather than claiming a link was sent.
		//
		// The generic "if that address exists, a link is on its way" answer
		// exists to stop account enumeration, and it is the right answer when
		// mail WORKS. When SMTP is unconfigured no link can ever arrive, so the
		// same words become a lie that leaves the user waiting for an email that
		// does not exist, with no way to tell that from a typo in their address.
		//
		// This reveals nothing about the account: it is a property of the
		// INSTANCE, identical for every address, and is already visible to anyone
		// who can read the deployment docs. Enumeration resistance is unaffected.
		slog.Warn("password reset requested but SMTP is not configured; told the user plainly",
			"email", email)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "password reset by email is not configured on this Flare instance. " +
				"Ask whoever operates it to set the SMTP_* variables, or to reset your password for you.",
		})
		return
	}

	token, err := id.Token(32)
	if err != nil {
		slogError(w, "reset token", err)
		return
	}
	if err := s.q.CreatePasswordResetToken(ctx, generated.CreatePasswordResetTokenParams{
		ID:        id.New(),
		UserID:    user.ID,
		TokenHash: auth.HashAPIKey(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		slogError(w, "create reset token", err)
		return
	}

	link := strings.TrimRight(s.cfg.BaseURL, "/") + "/reset-password?token=" + token
	go s.sendResetEmail(user.Email, link)
	ok()
}

func (s *Server) sendResetEmail(to, link string) {
	subject := "Reset your Flare password"
	text := fmt.Sprintf("Reset your Flare password with this link (valid for 1 hour):\n\n%s\n\nIf you did not request this, ignore this email.\n", link)
	body := fmt.Sprintf(`<div style="font:15px/1.6 -apple-system,Segoe UI,sans-serif;color:#18181b;max-width:520px">
<h2 style="margin:0 0 8px;font-size:18px">Reset your password</h2>
<p style="margin:0 0 16px;color:#52525b">Click below to set a new password. This link expires in 1 hour.</p>
<p style="margin:0 0 16px"><a href="%s" style="display:inline-block;background:#f59e0b;color:#18181b;text-decoration:none;padding:10px 18px;border-radius:6px;font-weight:600">Reset password</a></p>
<p style="margin:0;color:#a1a1aa;font-size:13px">If you did not request this, you can ignore this email.</p>
</div>`, html.EscapeString(link))
	if err := s.mailer.Send(to, subject, body, text); err != nil {
		slog.Warn("reset email delivery failed", "error", err)
	}
}

// handleResetPassword consumes a valid token and sets the new password.
//
// Rate limited per IP, before the body is parsed. /forgot-password above has had
// a limiter since it shipped and this route, the other half of the same flow,
// had none of any kind. That split is the whole bug: the mint was capped and the
// SPEND was not, and the spend is the half that runs bcrypt.
//
// Two costs sat open. Every request, token or no token, bought an
// unauthenticated Postgres lookup. And a request carrying a LIVE token bought a
// bcrypt at cost 12, the most expensive call in the binary. The live-token case
// is not the one-shot it looks like: there is no transaction here and the SELECT
// below takes no lock, so the whole bcrypt runs inside the window between
// reading the token and invalidating it. N concurrent requests replaying one
// forwarded link therefore all pass the lookup and all run bcrypt. One link out
// of a mailbox bought as many bcrypts as the caller could open sockets for.
//
// The key is the IP and NOTHING the caller sends. The token stays out of it,
// hashed or not, for the reason handleAcceptInvite spells out: a caller-chosen
// key component mints a fresh budget per request instead of capping anything.
// That also rules out reusing resetLimiter, whose key folds in an email address
// this request does not carry.
//
// What this does NOT defend is token guessing. The token is 32 random bytes, so
// entropy is the control there and this limit adds nothing to it. The cost is
// the point.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	// Before decodeJSON: a limit the body parser can skip is not a limit,
	// because an unparseable body is the cheapest thing an attacker can send.
	if !s.resetConsumeLimiter.Allow("reset-consume:" + clientIP(r)) {
		w.Header().Set("Retry-After", "900")
		writeErr(w, http.StatusTooManyRequests, "too many reset attempts, try again later")
		return
	}

	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	ctx := r.Context()
	prt, err := s.q.GetPasswordResetToken(ctx, auth.HashAPIKey(strings.TrimSpace(req.Token)))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid or expired reset link")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		slogError(w, "hash password", err)
		return
	}
	if err := s.q.UpdateUserPassword(ctx, generated.UpdateUserPasswordParams{ID: prt.UserID, PasswordHash: hash}); err != nil {
		slogError(w, "update password", err)
		return
	}
	// Invalidate every outstanding reset token for this user, not just the one
	// used, so a second leaked link cannot be redeemed after the reset.
	if err := s.q.InvalidatePasswordResetTokensForUser(ctx, prt.UserID); err != nil {
		slog.Warn("invalidate reset tokens", "error", err)
	}
	// Revoke every existing session for the user so an attacker who was already
	// logged in (the reason for the reset) is kicked out, not just locked out of
	// new logins.
	if err := s.q.BumpUserSessionsValidFrom(ctx, prt.UserID); err != nil {
		slog.Warn("bump sessions_valid_from", "error", err)
	}
	// Sessions alone are not the whole credential surface. An attacker who used
	// their access to mint an API key would otherwise keep it: the key survives
	// the reset, which is precisely the moment the victim believes they have
	// taken the account back. Revoke the keys THIS user created; org-wide
	// revocation would break every pipeline in the workspace over one person's
	// reset. Keys with no recorded creator predate migration 026 and are left
	// alone rather than guessed at.
	if n, err := s.q.DeleteAPIKeysCreatedBy(ctx, pgtype.Text{String: prt.UserID, Valid: true}); err != nil {
		slog.Warn("revoke api keys on password reset", "error", err)
	} else if n > 0 {
		slog.Info("revoked api keys on password reset", "user", prt.UserID, "count", n)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleChangePassword changes the signed-in user's password.
//
// Until now the ONLY way to change a password was the emailed reset link, so on
// an instance without SMTP there was no way at all, and even with SMTP a user
// who simply wanted to rotate their password had to pretend to have forgotten
// it. Both are the kind of gap that pushes people to reuse a password forever.
//
// The current password is required. A signed-in session is not sufficient
// authority to change the credential that session rests on: a borrowed laptop or
// a stolen cookie must not become a permanent account takeover.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Same floor the register and reset paths use.
	if len(req.NewPassword) < 8 {
		writeErr(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}

	ctx := r.Context()
	user, err := s.q.GetUserByID(ctx, userIDFrom(ctx))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Rate-limited on the USER, so a stolen cookie cannot be used to brute-force
	// the current password from inside a session.
	lockKey := "pwchange:" + user.ID
	if s.loginLimiter.Blocked(lockKey) {
		w.Header().Set("Retry-After", "300")
		writeErr(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}
	if !auth.VerifyPasswordConstantTime(user.PasswordHash, req.CurrentPassword) {
		s.loginLimiter.Record(lockKey)
		writeErr(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	s.loginLimiter.Reset(lockKey)

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		slogError(w, "hash password", err)
		return
	}
	if err := s.q.UpdateUserPassword(ctx, generated.UpdateUserPasswordParams{
		ID: user.ID, PasswordHash: hash,
	}); err != nil {
		slogError(w, "update password", err)
		return
	}
	// Revoke every OTHER session: whoever else was signed in as this user is the
	// reason people change passwords. Bumping the epoch invalidates this session
	// too, so re-establish it immediately and the user stays where they are.
	if err := s.q.BumpUserSessionsValidFrom(ctx, user.ID); err != nil {
		slog.Warn("bump sessions_valid_from on password change", "error", err)
	}
	s.establishSession(ctx, user.ID, user.OrgID)

	// API keys are deliberately NOT revoked here, unlike on a reset. A reset
	// implies the account was lost; a voluntary change does not, and silently
	// breaking the user's own CI would teach them not to rotate passwords.
	s.audit(ctx, "user.password_change", user.Email)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
