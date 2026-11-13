package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/bright-interaction/flare/internal/config"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/oidc"
)

func TestOIDCPrelinkedIdentityPreservesAccountAndRotatesSessionDB(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	uid := id.New()
	issuer := oidc.CloudRebellionIssuer
	_, err := pool.Exec(ctx, `INSERT INTO users(id,org_id,email,password_hash,role,sso_issuer,sso_subject) VALUES($1,$2,$3,$4,'member',$5,'bound-subject')`, uid, project.OrgID, "existing@example.test", "unchanged-password-hash", issuer)
	if err != nil {
		t.Fatal(err)
	}
	// The private repository owns an explicit operator workspace and singleton
	// retention policy (migrations 092/093). SSO must not transfer either.
	if _, err := pool.Exec(ctx, `UPDATE orgs SET is_operator=TRUE WHERE id=$1`, project.OrgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO retention_settings(singleton,hot_retention_days,cold_mode,updated_by) VALUES(TRUE,60,'forever',$1)`, uid); err != nil {
		t.Fatal(err)
	}
	var retentionBefore string
	if err := pool.QueryRow(ctx, `SELECT to_jsonb(r)::text FROM retention_settings r WHERE singleton`).Scan(&retentionBefore); err != nil {
		t.Fatal(err)
	}
	s := serverForDB(pool)
	s.cfg = config.Config{OIDCPrivateIssuer: issuer, OIDCPeerIP: "100.64.0.10", OIDCPrelinkedOnly: true}
	s.sessions = scs.New()
	cfg := &generated.OidcConfig{Issuer: issuer, ClientID: "client", Enabled: true, DefaultRole: "member"}
	for _, email := range []string{"", "unverified-rename@example.test"} {
		u, failure := s.resolveOIDCUser(ctx, project.OrgID, cfg, &oidc.UserInfo{Subject: "bound-subject", Email: email, EmailVerified: false})
		if failure != "" || u == nil || u.ID != uid {
			t.Fatalf("prelinked identity refused: %s", failure)
		}
	}
	var email, password, role, sub string
	if err := pool.QueryRow(ctx, `SELECT email,password_hash,role,sso_subject FROM users WHERE id=$1 AND org_id=$2`, uid, project.OrgID).Scan(&email, &password, &role, &sub); err != nil {
		t.Fatal(err)
	}
	if email != "existing@example.test" || password != "unchanged-password-hash" || role != "member" || sub != "bound-subject" {
		t.Fatal("prelinked login changed local account authority")
	}
	for _, info := range []*oidc.UserInfo{{Subject: "unknown", Email: email, EmailVerified: true}, {Subject: "unknown", Email: email, EmailVerified: false}} {
		if u, failure := s.resolveOIDCUser(ctx, project.OrgID, cfg, info); u != nil || failure != "sso_prelinked_required" {
			t.Fatal("pilot linked or provisioned an unknown identity")
		}
	}
	seeded := httptest.NewRecorder()
	s.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.sessions.Put(r.Context(), "anonymous", "fixture")
		w.WriteHeader(204)
	})).ServeHTTP(seeded, httptest.NewRequest("GET", "/", nil))
	original := seeded.Result().Cookies()[0]
	request := httptest.NewRequest("GET", "/", nil)
	request.AddCookie(original)
	logged := httptest.NewRecorder()
	s.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, failure := s.resolveOIDCUser(r.Context(), project.OrgID, cfg, &oidc.UserInfo{Subject: "bound-subject"})
		if failure != "" {
			t.Fatal(failure)
		}
		s.establishSession(r.Context(), u.ID, u.OrgID)
		if s.sessions.GetString(r.Context(), "user_id") != uid || s.sessions.GetString(r.Context(), "org_id") != project.OrgID || s.sessions.GetInt64(r.Context(), "auth_epoch") == 0 {
			t.Fatal("session identity or auth epoch missing")
		}
		w.WriteHeader(204)
	})).ServeHTTP(logged, request)
	if len(logged.Result().Cookies()) == 0 || logged.Result().Cookies()[0].Value == original.Value {
		t.Fatal("SSO did not rotate the browser session")
	}
	var operator bool
	var retentionAfter string
	if err := pool.QueryRow(ctx, `SELECT is_operator FROM orgs WHERE id=$1`, project.OrgID).Scan(&operator); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT to_jsonb(r)::text FROM retention_settings r WHERE singleton`).Scan(&retentionAfter); err != nil {
		t.Fatal(err)
	}
	if !operator || retentionAfter != retentionBefore {
		t.Fatal("prelinked login changed operator or retention authority")
	}
}

func TestOIDCGenericVerifiedEmailAndCrossOrgControlsDB(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	other := seedProjectForIngest(t, pool)
	uid := id.New()
	_, err := pool.Exec(ctx, `INSERT INTO users(id,org_id,email,password_hash,role) VALUES($1,$2,$3,'preserved','member')`, uid, project.OrgID, "generic@example.test")
	if err != nil {
		t.Fatal(err)
	}
	s := serverForDB(pool)
	cfg := &generated.OidcConfig{Issuer: "https://generic.example", ClientID: "client", DefaultRole: "member", Enabled: true}
	if u, reason := s.resolveOIDCUser(ctx, project.OrgID, cfg, &oidc.UserInfo{Subject: "generic-sub", Email: "generic@example.test"}); u != nil || reason != "sso_unverified" {
		t.Fatal("generic unverified email matched a local account")
	}
	var binding string
	if err := pool.QueryRow(ctx, `SELECT sso_subject FROM users WHERE id=$1 AND org_id=$2`, uid, project.OrgID).Scan(&binding); err != nil || binding != "" {
		t.Fatal("unverified generic login mutated the binding")
	}
	u, reason := s.resolveOIDCUser(ctx, project.OrgID, cfg, &oidc.UserInfo{Subject: "generic-sub", Email: "generic@example.test", EmailVerified: true})
	if reason != "" || u == nil || u.ID != uid {
		t.Fatalf("existing generic verified-email linking changed: %s", reason)
	}
	if u, reason := s.resolveOIDCUser(ctx, other.OrgID, cfg, &oidc.UserInfo{Subject: "generic-sub"}); u != nil || reason != "sso_other_org" {
		t.Fatal("bound identity crossed organizations")
	}
	u, reason = s.resolveOIDCUser(ctx, project.OrgID, cfg, &oidc.UserInfo{Subject: "new-generic-sub", Email: "new-generic@example.test", EmailVerified: true})
	if reason != "" || u == nil || u.Role != "member" {
		t.Fatalf("existing generic JIT changed: %s", reason)
	}
}

func TestOIDCTransactionsConsumeOnceAndRejectConfigurationSwitchDB(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	project := seedProjectForIngest(t, pool)
	s := serverForDB(pool)
	s.cfg = config.Config{SecretKey: strings.Repeat("k", 32), BaseURL: "https://flare.example", Environment: "production"}
	proof := txProof()
	proof.Org = project.OrgID
	proof.RedirectURI = s.oidcRedirectURI()
	if err := s.storeOIDCTransaction(ctx, proof); err != nil {
		t.Fatal(err)
	}
	wrong := proof
	wrong.ClientID = "changed"
	if s.consumeOIDCTransaction(ctx, wrong) {
		t.Fatal("changed client consumed an original transaction")
	}
	if !s.consumeOIDCTransaction(ctx, proof) || s.consumeOIDCTransaction(ctx, proof) {
		t.Fatal("transaction is not atomically one-use")
	}
	concurrent := proof
	concurrent.State = oidc.RandomState()
	if err := s.storeOIDCTransaction(ctx, concurrent); err != nil {
		t.Fatal(err)
	}
	results := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- s.consumeOIDCTransaction(ctx, concurrent) }()
	}
	accepted := 0
	for i := 0; i < 2; i++ {
		if <-results {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatal("concurrent replay consumed transaction more than once")
	}
	for _, change := range []string{"issuer", "client", "redirect"} {
		s.cfg.BaseURL = "https://flare.example"
		current := proof
		current.State = oidc.RandomState()
		if err := s.storeOIDCTransaction(ctx, current); err != nil {
			t.Fatal(err)
		}
		issuer, client := current.Issuer, current.ClientID
		switch change {
		case "issuer":
			issuer = "https://changed.example"
		case "client":
			client = "changed"
		case "redirect":
			s.cfg.BaseURL = "https://changed-flare.example"
		}
		if err := s.q.UpsertOIDCConfig(ctx, generated.UpsertOIDCConfigParams{OrgID: project.OrgID, Issuer: issuer, ClientID: client, ClientSecret: "unused", DefaultRole: "member", Enabled: true}); err != nil {
			t.Fatal(err)
		}
		// Equality is checked before any provider request, and consumes this proof.
		start := httptest.NewRecorder()
		s.setOIDCTx(start, current)
		request := httptest.NewRequest("GET", "/api/auth/oidc/callback?state="+current.State+"&code=fixture-code", nil)
		request.AddCookie(start.Result().Cookies()[0])
		response := httptest.NewRecorder()
		s.handleOIDCCallback(response, request)
		if response.Header().Get("Location") != "/login?error=sso_disabled" {
			t.Fatalf("%s configuration switch accepted", change)
		}
		if s.consumeOIDCTransaction(ctx, current) {
			t.Fatal("rejected configuration switch remained replayable")
		}
	}
	expired := proof
	expired.State = strings.Repeat("x", 43)
	expired.Expires = time.Now().Add(-time.Minute).Unix()
	if err := s.storeOIDCTransaction(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if s.consumeOIDCTransaction(ctx, expired) {
		t.Fatal("expired transaction consumed")
	}
}
