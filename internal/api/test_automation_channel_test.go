package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/flare/internal/alerts"
	"github.com/bright-interaction/flare/internal/ratelimit"
	"github.com/bright-interaction/flare/internal/secretbox"
)

func automationTestRequest(s *Server, org, chID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/channels/"+chID+"/test", strings.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", chID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, ctxOrgID, org)
	rec := httptest.NewRecorder()
	s.handleTestChannel(rec, req.WithContext(ctx))
	return rec
}

func TestAutomationChannelTestRequiresRoutedProject(t *testing.T) {
	pool := newTestDB(t)
	org, chID, projectIDs := seedOrgProjectChannel(t, pool, 2)
	otherOrg, _, otherProjects := seedOrgProjectChannel(t, pool, 1)
	if otherOrg == org {
		t.Fatal("expected two distinct orgs")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE notification_channels SET type = 'automation_webhook', config = '{}' WHERE id = $1`, chID); err != nil {
		t.Fatalf("configure automation channel: %v", err)
	}
	setRouting(t, pool, chID, projectIDs[0])
	s := serverForDB(pool)
	s.testLimiter = ratelimit.New(20, time.Minute)
	s.dispatcher = alerts.NewDispatcher(nil)
	s.secrets = secretbox.New("")

	for _, tc := range []struct {
		name, org, body string
		wantStatus      int
	}{
		{"missing project", org, `{}`, http.StatusBadRequest},
		{"invalid project", org, `{"project_id":"missing"}`, http.StatusNotFound},
		{"cross tenant project", org, fmt.Sprintf(`{"project_id":%q}`, otherProjects[0]), http.StatusNotFound},
		{"unrouted project", org, fmt.Sprintf(`{"project_id":%q}`, projectIDs[1]), http.StatusBadRequest},
		{"cross tenant channel", otherOrg, fmt.Sprintf(`{"project_id":%q}`, projectIDs[0]), http.StatusNotFound},
		{"routed project reaches delivery", org, fmt.Sprintf(`{"project_id":%q}`, projectIDs[0]), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := automationTestRequest(s, tc.org, chID, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.name == "routed project reaches delivery" {
				// The fixture intentionally has no destination credentials. A
				// delivery failure proves the routed request passed validation;
				// it must not claim a successful test send.
				var outcome struct {
					OK bool `json:"ok"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &outcome); err != nil || outcome.OK {
					t.Fatalf("test must report delivery failure, got %s (decode: %v)", rec.Body.String(), err)
				}
			}
		})
	}
}
