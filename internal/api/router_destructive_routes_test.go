package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestDestructiveRoutesAreAdminOnly keeps irreversible and credential routes out
// of reach of an API key.
//
// Giving keys their own role fixed the DEFAULT (new keys are read-only) and left
// these routes inside the member group, so a key minted for CI provisioning or
// source-map upload still carried DELETE /projects/{id}, which erases a project
// and all of its telemetry in one transaction, plus the ability to mint and
// revoke keys including its own successors.
//
// apiKeyRole clamps a key to viewer or member and refuses anything else, so
// admin is unreachable by any key. Moving these three routes there is what makes
// that clamp mean something.
//
// This is a source assertion because the property is about ROUTER SHAPE: which
// middleware group a line sits in. That is invisible to a handler-level test and
// is exactly the kind of thing that drifts back when someone adds a route next
// to a similar one.
func TestDestructiveRoutesAreAdminOnly(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	body := string(src)

	// The member group runs from its requireRole("member") to the next
	// requireRole, which is where the admin block begins.
	memberStart := strings.Index(body, `s.requireRole("member")`)
	if memberStart < 0 {
		t.Fatal(`no requireRole("member") group found in router.go`)
	}
	rest := body[memberStart+1:]
	next := regexp.MustCompile(`s\.requireRole\("(admin|owner)"\)`).FindStringIndex(rest)
	if next == nil {
		t.Fatal("no admin/owner group after the member group; the router shape has changed")
	}
	memberBlock := rest[:next[0]]

	for _, route := range []struct{ handler, why string }{
		{"handleDeleteProject", "erases a project and all of its telemetry in one transaction"},
		{"handleRetireSourceMapBundle", "erases an immutable source map bundle and needs a human operator"},
		{"handleCreateAPIKey", "lets a leaked key mint its own successors"},
		{"handleDeleteAPIKey", "lets a leaked key revoke the keys that would replace it"},
	} {
		if strings.Contains(memberBlock, route.handler) {
			t.Errorf("%s is in the member group, so any org API key can call it: %s. "+
				"Move it to requireRole(\"admin\"), which apiKeyRole makes unreachable by a key.",
				route.handler, route.why)
		}
	}
	if !strings.Contains(rest[next[0]:], `r.Delete("/projects/{id}/artifact-bundles/{bundleID}", s.handleRetireSourceMapBundle)`) {
		t.Fatal("bundle retirement route is missing from the admin-only router block")
	}
}
