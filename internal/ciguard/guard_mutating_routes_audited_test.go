package ciguard

// Structural backstop for the "some mutations are recorded and some are not"
// family (audit finding M19, 2026-08-11).
//
// The audit swept all 21 s.audit call sites against every mutating route and
// found the coverage was arbitrary: channel.update was audited, channel.create
// and channel.delete were not. That is the wrong way round. A compromised
// member creating a webhook channel pointed at their own endpoint mirrors every
// alert in the org, including the sensitive-data-in-payload security events,
// and the audit log an admin reads at GET /api/audit-log showed nothing.
// Deleting it afterwards was equally invisible. The one operation on that
// resource that WAS recorded is the least sensitive one.
//
// So the rule is enforced rather than remembered: a non-GET route registered
// inside a requireRole group either calls s.audit, writes a transactional
// InsertAuditLog row, or appears in the exemption table below with a reason.
// Adding a route makes the build tell you which.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// exemptFromAudit lists mutating routes that deliberately write no audit row.
// A new entry needs a reason that survives being read out loud in an incident.
var exemptFromAudit = map[string]string{
	"handleUpdateIssueStatus": "issue workflow state, changed constantly during triage; the issue itself carries its status and history",
	"handleTriageIssue":       "audited one level down, inside triageIssue, so all three triage surfaces record through one call",
	"handleCreateRelease":     "written by CI on every deploy; volume would drown the log an admin reads",
	"handleDeleteOrg":         "audit_log is FK-linked to orgs, so the cascade deletes the evidence; recorded as a slog line instead",
	"handleLogout":            "ends the caller's own session, no state another member can observe",
}

// These security state mutations commit an audit row in the same transaction.
// Their DB tests prove that an unavailable audit table rolls the mutation back.
// Pin actor, scope and target so an unrelated InsertAuditLog cannot satisfy the
// route guard.
var transactionalAuditActions = map[string]struct {
	action, org, actor, target, commit string
}{
	"handleCreateBusinessCheckKey": {"business_key.create", "project.OrgID", "businessKeyAuditActor(r.Context())", "project.ID +", "tx.Commit("},
	"handleRevokeBusinessCheckKey": {"business_key.revoke", "project.OrgID", "businessKeyAuditActor(r.Context())", "project.ID +", "tx.Commit("},
	"handleHubSpotOAuthStart":      {"connector.oauth_start", "orgIDFrom(r.Context())", "pgText(userIDFrom(r.Context()))", "req.ProjectID", "dbtx.Commit("},
}

// Receipt verification delegates the mutation to a helper that commits its
// proof, status change and audit row together. Pin both sides of the call so a
// route cannot borrow an unrelated helper's audit statement.
var delegatedTransactionalAuditActions = map[string]struct {
	helper string
	action string
}{
	"handleVerifyAutomationReceipt": {"promoteStageReceipt", "automation.receipt.verify"},
}

var (
	reRequireRole = regexp.MustCompile(`requireRole\("(\w+)"\)`)
	reMutating    = regexp.MustCompile(`r\.(Post|Put|Patch|Delete)\("[^"]*",\s*s\.(\w+)\)`)
)

func TestMutatingRoleGatedRoutesAreAudited(t *testing.T) {
	router, err := os.ReadFile("../api/router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	// Everything from the first requireRole group onward is role-gated. The
	// unauthenticated and ingest routes sit above it and are not in scope: they
	// are authenticated by a DSN key or by nothing, so there is no actor to
	// attribute an action to.
	src := string(router)
	first := reRequireRole.FindStringIndex(src)
	if first == nil {
		t.Fatal("no requireRole group found in router.go; this guard needs rewiring")
	}
	handlers := map[string]bool{}
	for _, m := range reMutating.FindAllStringSubmatch(src[first[0]:], -1) {
		handlers[m[2]] = true
	}
	if len(handlers) == 0 {
		t.Fatal("no mutating role-gated routes found; this guard is no longer looking at anything real")
	}

	bodies, err := handlerBodies("../api")
	if err != nil {
		t.Fatalf("read handlers: %v", err)
	}
	for name := range handlers {
		if reason, ok := exemptFromAudit[name]; ok {
			if reason == "" {
				t.Errorf("%s is exempt from auditing with no reason given", name)
			}
			continue
		}
		body, ok := bodies[name]
		if !ok {
			t.Errorf("%s is routed but its definition was not found; this guard cannot see it", name)
			continue
		}
		if rule, transactional := transactionalAuditActions[name]; transactional {
			for _, required := range []string{
				"qtx.InsertAuditLog(", `Action: "` + rule.action + `"`,
				"OrgID: " + rule.org, "ActorUserID: " + rule.actor,
				"Target: " + rule.target, rule.commit,
			} {
				if !strings.Contains(body, required) {
					t.Errorf("%s must commit its %q audit row in the mutation transaction; missing %q", name, rule.action, required)
				}
			}
			continue
		}
		if delegated, transactional := delegatedTransactionalAuditActions[name]; transactional {
			helperBody, found := bodies[delegated.helper]
			if !found || !strings.Contains(body, "s."+delegated.helper+"(") {
				t.Errorf("%s does not call transactional audit helper %s", name, delegated.helper)
				continue
			}
			for _, required := range []string{
				"INSERT INTO audit_log", `"` + delegated.action + `"`, "b.OrgID", "tx.Commit(ctx)",
			} {
				if !strings.Contains(helperBody, required) {
					t.Errorf("%s helper %s must commit scoped %q audit row; missing %q", name, delegated.helper, delegated.action, required)
				}
			}
			continue
		}
		if !strings.Contains(body, "s.audit(") {
			t.Errorf("%s mutates inside a requireRole group and writes no audit row.\n"+
				"Add s.audit(ctx, \"<resource>.<verb>\", target), use a checked transactional audit, or add it to exemptFromAudit "+
				"with a reason.", name)
		}
	}
}

// Keep exception and transactional-audit entries from rotting: a row for a
// handler that no longer exists is a comment pretending to be a control.
func TestAuditExemptionsAreStillRouted(t *testing.T) {
	bodies, err := handlerBodies("../api")
	if err != nil {
		t.Fatalf("read handlers: %v", err)
	}
	for name := range exemptFromAudit {
		if _, ok := bodies[name]; !ok {
			t.Errorf("exemptFromAudit lists %s, which no longer exists", name)
		}
	}
	for name := range transactionalAuditActions {
		if _, ok := bodies[name]; !ok {
			t.Errorf("transactionalAuditActions lists %s, which no longer exists", name)
		}
	}
	for name, delegated := range delegatedTransactionalAuditActions {
		if _, ok := bodies[name]; !ok {
			t.Errorf("delegatedTransactionalAuditActions lists %s, which no longer exists", name)
		}
		if _, ok := bodies[delegated.helper]; !ok {
			t.Errorf("delegatedTransactionalAuditActions lists missing helper %s", delegated.helper)
		}
	}
}

// handlerBodies maps server methods to their own source text. Parsing the Go
// declarations prevents a handler from borrowing a neighbouring method's
// audit statement (including non-HTTP helpers in the same file).
func handlerBodies(dir string) (map[string]string, error) {
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(dir + "/" + e.Name())
		if err != nil {
			return nil, err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, e.Name(), raw, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, declaration := range file.Decls {
			method, ok := declaration.(*ast.FuncDecl)
			if !ok || method.Recv == nil || method.Body == nil || len(method.Recv.List) != 1 {
				continue
			}
			receiver, ok := method.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			typeName, ok := receiver.X.(*ast.Ident)
			if !ok || typeName.Name != "Server" {
				continue
			}
			start, end := fset.Position(method.Pos()).Offset, fset.Position(method.End()).Offset
			out[method.Name.Name] = string(raw[start:end])
		}
	}
	return out, nil
}
