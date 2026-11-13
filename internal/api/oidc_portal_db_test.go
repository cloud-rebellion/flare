package api

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/bright-interaction/flare/internal/config"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/oidc"
)

// These tests require the existing FLARE_TEST_DATABASE_URL fixture. Every
// account, org, transaction and database belongs to newTestDB, never a service.
func TestPortalSelectedContextChangesRefuseBeforeAccountEffectsDB(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	uid := id.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,org_id,email,password_hash,role,sso_issuer,sso_subject)
		VALUES($1,$2,'preserved@example.test','unchanged-password','viewer','https://127.0.0.1','bound-subject')`, uid, project.OrgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orgs SET is_operator=TRUE WHERE id=$1`, project.OrgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO retention_settings(singleton,hot_retention_days,cold_mode,updated_by) VALUES(TRUE,60,'forever',$1)`, uid); err != nil {
		t.Fatal(err)
	}
	s := serverForDB(pool)
	s.sessions = scs.New()
	base := config.Config{SecretKey: strings.Repeat("k", 32), BaseURL: "https://flare.example", Environment: "production",
		OIDCLaunchOrgID: project.OrgID, OIDCRequiredACR: "urn:fixture:uv", OIDCRequiredAMR: "pop,mfa", OIDCPrelinkedOnly: true,
		OIDCPrivateIssuer: "https://127.0.0.1"}
	params := generated.UpsertOIDCConfigParams{OrgID: project.OrgID, Issuer: "https://127.0.0.1", ClientID: "fixture-client",
		ClientSecret: "fixture-stored-secret", DefaultRole: "member", Enabled: true}
	// The issuer is deliberately a refused loopback literal. A misplaced guard
	// would reach discovery and return sso_discovery, rather than the required
	// sso_state. No fixture network, provider token or private peer is needed.
	snapshot := func() [32]byte {
		t.Helper()
		var accounts, orgs, retention string
		for query, dest := range map[string]*string{
			`SELECT coalesce(jsonb_agg(to_jsonb(u) ORDER BY id),'[]')::text FROM users u`:  &accounts,
			`SELECT coalesce(jsonb_agg(to_jsonb(o) ORDER BY id),'[]')::text FROM orgs o`:   &orgs,
			`SELECT coalesce(jsonb_agg(to_jsonb(r)),'[]')::text FROM retention_settings r`: &retention,
		} {
			if err := pool.QueryRow(ctx, query).Scan(dest); err != nil {
				t.Fatal(err)
			}
		}
		return sha256.Sum256([]byte(accounts + "\x00" + orgs + "\x00" + retention))
	}
	before := snapshot()
	for _, change := range []string{"acr", "amr", "incomplete", "selector", "missing-context", "client-secret", "default-role", "private-policy"} {
		t.Run(change, func(t *testing.T) {
			s.cfg = base
			if err := s.q.UpsertOIDCConfig(ctx, params); err != nil {
				t.Fatal(err)
			}
			cfg, err := s.q.GetOIDCConfig(ctx, project.OrgID)
			if err != nil {
				t.Fatal(err)
			}
			profile, err := s.oidcAssurance(project.OrgID)
			if err != nil {
				t.Fatal(err)
			}
			proof := txProof()
			proof.State, proof.Org, proof.Issuer, proof.ClientID = oidc.RandomState(), project.OrgID, cfg.Issuer, cfg.ClientID
			proof.RedirectURI, proof.Assurance = s.oidcRedirectURI(), s.oidcAssuranceBinding(project.OrgID, cfg, profile)
			if change == "missing-context" {
				proof.Assurance = "" // An actual pending legacy transaction cannot adopt an enabled profile.
			}
			if err := s.storeOIDCTransaction(ctx, proof); err != nil {
				t.Fatal(err)
			}
			cookie := httptest.NewRecorder()
			s.setOIDCTx(cookie, proof)
			switch change {
			case "acr":
				s.cfg.OIDCRequiredACR = "urn:fixture:different"
			case "amr":
				s.cfg.OIDCRequiredAMR = "pop,mfa,extra"
			case "incomplete":
				s.cfg.OIDCRequiredAMR = ""
			case "selector":
				s.cfg.OIDCLaunchOrgID = "another-org"
			case "client-secret", "default-role":
				updated := params
				if change == "client-secret" {
					updated.ClientSecret = "different-stored-secret"
				} else {
					updated.DefaultRole = "admin"
				}
				if err := s.q.UpsertOIDCConfig(ctx, updated); err != nil {
					t.Fatal(err)
				}
			case "private-policy":
				s.cfg.OIDCPrelinkedOnly = false
			}
			request := carry(t, cookie)
			request.URL.RawQuery = url.Values{"state": {proof.State}, "code": {"fixture-code"}}.Encode()
			result := httptest.NewRecorder()
			s.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s.sessions.Put(r.Context(), "anonymous", "preserved")
				s.handleOIDCCallback(w, r)
				if s.sessions.GetString(r.Context(), "user_id") != "" || s.sessions.GetString(r.Context(), "anonymous") != "preserved" {
					t.Fatal("refused callback established or replaced a session")
				}
			})).ServeHTTP(result, request)
			if result.Code != http.StatusFound || result.Header().Get("Location") != "/login?error=sso_state" {
				t.Fatalf("changed context reached effects or discovery: status %d location %q", result.Code, result.Header().Get("Location"))
			}
			if s.consumeOIDCTransaction(ctx, proof) {
				t.Fatal("refused callback left its transaction replayable")
			}
			if snapshot() != before {
				t.Fatal("refused callback changed local account, credential, operator or retention authority")
			}
		})
	}
}

func TestPortalUnrelatedOrganizationKeepsLegacyTransactionAndPrelinkRuleDB(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	s.cfg = config.Config{OIDCLaunchOrgID: "different-selected-org", OIDCRequiredACR: "incomplete-profile",
		OIDCPrivateIssuer: "https://same-issuer.example", OIDCPrelinkedOnly: true}
	profile, err := s.oidcAssurance(project.OrgID)
	if err != nil || profile.Enabled() {
		t.Fatal("selected incomplete profile changed an unrelated organization")
	}
	proof := txProof()
	proof.Org, proof.Issuer = project.OrgID, s.cfg.OIDCPrivateIssuer
	cfg := &generated.OidcConfig{Issuer: proof.Issuer, ClientID: proof.ClientID, Enabled: true, DefaultRole: "member"}
	proof.Assurance = s.oidcAssuranceBinding(project.OrgID, cfg, profile)
	legacy := sha256.Sum256([]byte(proof.Issuer + "\x00" + proof.ClientID + "\x00" + proof.RedirectURI))
	if proof.Assurance != "" || proof.configHash() != legacy {
		t.Fatal("unrelated same-issuer organization no longer has the exact legacy hash")
	}
	if err := s.storeOIDCTransaction(ctx, proof); err != nil {
		t.Fatal(err)
	}
	if !s.consumeOIDCTransaction(ctx, proof) || s.consumeOIDCTransaction(ctx, proof) {
		t.Fatal("unrelated organization's original one-use transaction changed")
	}
	if user, reason := s.resolveOIDCUser(ctx, project.OrgID, cfg, &oidc.UserInfo{Subject: "unbound", Email: "unbound@example.test", EmailVerified: true}); user != nil || reason != "sso_prelinked_required" {
		t.Fatal("new org selector weakened the existing private-issuer prelink restriction")
	}
}

func TestPortalNativeCrossTabSessionWinsAndRevocationRemainsLocalDB(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	uid := id.New()
	epoch := time.Now().Unix() - 60
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,org_id,email,password_hash,role,sessions_valid_from) VALUES($1,$2,'native@example.test','unchanged-password','viewer',to_timestamp($3))`, uid, project.OrgID, epoch-1); err != nil {
		t.Fatal(err)
	}
	s := serverForDB(pool)
	s.sessions = scs.New()
	// Even invalid SSO configuration or hostile caller parameters cannot displace
	// a valid current native session. The selected workspace is not inferred.
	s.cfg = config.Config{OIDCLaunchOrgID: "invalid selector", OIDCRequiredACR: "incomplete"}
	invoke := func(handler http.HandlerFunc) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		s.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.sessions.Put(r.Context(), "user_id", uid)
			s.sessions.Put(r.Context(), "org_id", "stale-org")
			s.sessions.Put(r.Context(), "auth_epoch", epoch)
			handler(w, r)
		})).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/launch?org=other&return_url=https://other.example", nil))
		return w
	}
	launched := invoke(s.handleOIDCLaunch)
	if launched.Code != http.StatusFound || launched.Header().Get("Location") != "/projects" {
		t.Fatal("current native session did not win the server-side launch recheck")
	}
	me := invoke(s.handleMe)
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), `"org_id":"`+project.OrgID+`"`) || !strings.Contains(me.Body.String(), `"role":"viewer"`) {
		t.Fatal("native session probe did not return current local account authority")
	}
	protected := invoke(func(w http.ResponseWriter, r *http.Request) {
		s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if orgIDFrom(r.Context()) != project.OrgID || roleFrom(r.Context()) != "viewer" {
				t.Fatal("protected request trusted stale session org or role")
			}
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(w, r)
	})
	if protected.Code != http.StatusNoContent {
		t.Fatal("native session no longer reaches protected routes")
	}
	var transactions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM oidc_login_transactions`).Scan(&transactions); err != nil || transactions != 0 {
		t.Fatal("native launcher created an OIDC transaction")
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET sessions_valid_from=now() WHERE id=$1`, uid); err != nil {
		t.Fatal(err)
	}
	if revoked := invoke(s.handleMe); revoked.Code != http.StatusUnauthorized {
		t.Fatal("local reset epoch no longer revokes the native session")
	}
	var password, role string
	if err := pool.QueryRow(ctx, `SELECT password_hash,role FROM users WHERE id=$1`, uid).Scan(&password, &role); err != nil || password != "unchanged-password" || role != "viewer" {
		t.Fatal("launch or revocation changed local credential or role")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid); err != nil {
		t.Fatal(err)
	}
	if removed := invoke(s.handleMe); removed.Code != http.StatusUnauthorized {
		t.Fatal("an actually removed user was not classified as signed out")
	}
	status := httptest.NewRecorder()
	s.handleOIDCLaunchStatus(status, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/launch-status?org="+project.OrgID, nil))
	if status.Code != http.StatusOK || strings.TrimSpace(status.Body.String()) != `{"ready":false}` {
		t.Fatal("caller parameters selected an otherwise unready launch profile")
	}
}

func TestPortalReadinessChoosesOnlyConfiguredPrimaryKeyDB(t *testing.T) {
	pool := newTestDB(t)
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	s.cfg = config.Config{OIDCLaunchOrgID: id.New(), OIDCRequiredACR: "urn:fixture:uv", OIDCRequiredAMR: "pop,mfa"}
	if err := s.q.UpsertOIDCConfig(context.Background(), generated.UpsertOIDCConfigParams{OrgID: project.OrgID,
		Issuer: "https://127.0.0.1", ClientID: "fixture-client", ClientSecret: "fixture-stored-secret", DefaultRole: "member", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	for _, selected := range []string{s.cfg.OIDCLaunchOrgID, project.OrgID} {
		s.cfg.OIDCLaunchOrgID = selected
		result := httptest.NewRecorder()
		s.handleOIDCLaunchStatus(result, httptest.NewRequest(http.MethodGet,
			"/api/auth/oidc/launch-status?org="+project.OrgID+"&org_id="+project.OrgID+"&return_url=https://other.example", nil))
		if result.Code != http.StatusOK || strings.TrimSpace(result.Body.String()) != `{"ready":false}` || result.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing configured primary key or disabled stored config became ready from caller input")
		}
	}
}
