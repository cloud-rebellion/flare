package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNativeSessionDatabaseFailureIsUnknownAndPreservesSession(t *testing.T) {
	// A closed real pool deterministically exercises an operational DB error
	// without a server, socket dial, fake row result or production data.
	pool, err := pgxpool.New(context.Background(), "postgres://fixture@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	s := serverForDB(pool)
	s.sessions = scs.New()
	for name, handler := range map[string]http.HandlerFunc{
		"me": s.handleMe, "launcher": s.handleOIDCLaunch,
		"protected": func(w http.ResponseWriter, r *http.Request) {
			s.requireAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unknown session reached protected handler") })).ServeHTTP(w, r)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s.sessions.Put(r.Context(), "user_id", "known-native-user")
				s.sessions.Put(r.Context(), "org_id", "known-native-org")
				s.sessions.Put(r.Context(), "auth_epoch", int64(123))
				handler(w, r)
				if s.sessions.GetString(r.Context(), "user_id") != "known-native-user" || s.sessions.GetInt64(r.Context(), "auth_epoch") != 123 {
					t.Fatal("DB failure cleared or replaced native session")
				}
			})).ServeHTTP(w, httptest.NewRequest("GET", "/api/auth/me", nil))
			if w.Code != http.StatusServiceUnavailable || w.Header().Get("Location") != "" {
				t.Fatalf("unknown session status=%d became signed-out or redirected", w.Code)
			}
		})
	}
}

func TestNativeSessionMissingIdentityIsActuallySignedOut(t *testing.T) {
	s := &Server{sessions: scs.New()}
	w := httptest.NewRecorder()
	s.sessions.LoadAndSave(http.HandlerFunc(s.handleMe)).ServeHTTP(w, httptest.NewRequest("GET", "/api/auth/me", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing native session status=%d", w.Code)
	}
}
