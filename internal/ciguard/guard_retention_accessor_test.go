package ciguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Effective retention has one source: Server.RetentionPolicy / retentionPosture
// in internal/api/retention_handlers.go, which resolve the stored dashboard
// policy over the env. cfg.RetentionDays, cfg.ColdRetention* and
// cfg.AllowDropWithoutExport are env FALLBACKS. When the stored policy landed,
// five surfaces (readiness, analytics health, source-map retirement, the
// settings view's allow-drop state, the log window) kept reading the env and
// reported 30 days archived while a stored "7 days, no archive" was dropping.
//
// This replaces a grep in scripts/ci.sh that exempted the whole of
// retention_handlers.go (a new function there could read the env freely) and
// passed when grep itself failed (fix audit N, O). Exemptions are by FUNCTION,
// and anything it cannot read is a failure.
var retentionEnvFields = map[string]bool{
	"RetentionDays":          true,
	"ColdRetentionMode":      true,
	"ColdRetentionDays":      true,
	"AllowDropWithoutExport": true,
}

var retentionEnvReaders = map[string]string{
	"envRetention": "builds the env fallback policy",
	"postureFor":   "attaches the allow-drop flag to an effective policy",
}

// retentionEnvReads returns "file:line func field" for every X.cfg.<field>
// read outside the exempt functions.
func retentionEnvReads(fset *token.FileSet, f *ast.File) []string {
	var hits []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		name := ""
		if ok {
			name = fn.Name.Name
		}
		if _, exempt := retentionEnvReaders[name]; exempt && ok && fn.Recv != nil {
			continue
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !retentionEnvFields[sel.Sel.Name] {
				return true
			}
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "cfg" {
				hits = append(hits, fset.Position(sel.Pos()).String()+" "+name+" cfg."+sel.Sel.Name)
			}
			return true
		})
	}
	return hits
}

func TestEffectiveRetentionHasOneAccessor(t *testing.T) {
	const dir = "../api"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	scanned, sawAccessor := 0, 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, dir+"/"+e.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		scanned++
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil {
				if _, exempt := retentionEnvReaders[fn.Name.Name]; exempt {
					sawAccessor++
				}
			}
		}
		for _, hit := range retentionEnvReads(fset, f) {
			t.Errorf("%s: read effective retention through s.RetentionPolicy / s.retentionPosture, not s.cfg", hit)
		}
	}
	if scanned == 0 {
		t.Fatal("no internal/api sources scanned; this guard is looking at nothing")
	}
	if sawAccessor != len(retentionEnvReaders) {
		t.Fatalf("found %d of the %d exempt accessor functions; update retentionEnvReaders", sawAccessor, len(retentionEnvReaders))
	}
}

// The guard must catch a read in a NEW function of the accessor's own file,
// which the old file-level grep exemption let through.
func TestRetentionAccessorGuardCatchesASeededViolation(t *testing.T) {
	src := `package api
func (s *Server) envRetention() int { return s.cfg.RetentionDays }
func (s *Server) sneaky() int { return s.cfg.RetentionDays + boolInt(s.cfg.AllowDropWithoutExport) }`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "retention_handlers.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hits := retentionEnvReads(fset, f); len(hits) != 2 || !strings.Contains(hits[0], "sneaky") {
		t.Fatalf("seeded violation: %v, want the two reads in sneaky", hits)
	}
}
