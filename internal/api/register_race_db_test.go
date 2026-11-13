package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bright-interaction/flare/internal/auth"
	"github.com/bright-interaction/flare/internal/ratelimit"
)

// Two registrations racing for one address used to answer 500 to the loser:
// the unique violation surfaced as an internal error (fix audit L). It is the
// same "email already registered" 409 the sequential path answers.
func TestConcurrentRegistrationLoserGets409(t *testing.T) {
	pool := newTestDB(t)
	s := serverForDB(pool)
	s.cfg.BaseURL = "https://flare.example"
	s.cfg.AllowSignup = true
	s.sessions = auth.NewSessionManager(time.Hour, time.Hour, false)
	s.signupLimiter = ratelimit.New(10000, time.Hour)
	handler := s.Routes(fstest.MapFS{"index.html": {Data: []byte("test")}}, func(next http.Handler) http.Handler { return next })
	register := func(email string) int {
		r := httptest.NewRequest("POST", "https://flare.example/api/auth/register",
			strings.NewReader(fmt.Sprintf(`{"email":%q,"password":"password123","org_name":"Org"}`, email)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	counts := map[int]int{}
	for i := 0; i < 25; i++ {
		email := fmt.Sprintf("race%d@synthetic.invalid", i)
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for j := range codes {
			wg.Add(1)
			go func(j int) { defer wg.Done(); codes[j] = register(email) }(j)
		}
		wg.Wait()
		for _, c := range codes {
			counts[c]++
		}
	}
	if counts[http.StatusInternalServerError] > 0 || counts[http.StatusCreated] != 25 || counts[http.StatusConflict] != 25 {
		t.Fatalf("25 racing pairs answered %v, want 25x201 and 25x409 and no 500", counts)
	}
}
