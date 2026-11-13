package ciguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Every enumeration of, and deletion inside, the Parquet archive goes through
// the shared walker in internal/analytics/archive_walk.go, which works
// fd-relative through os.Root (fix audit F/G). The P2-4 symlink fix landed in
// the prune and missed the purge because the two walked the archive with
// their own path code. So path-based deletion and globbing are banned in the
// whole package: there is no exemption, the walker itself uses os.Root.
var archivePathCalls = map[string]bool{
	"filepath.Glob": true,
	"os.Remove":     true,
	"os.RemoveAll":  true,
}

func findArchivePathCalls(fset *token.FileSet, file *ast.File) []string {
	var hits []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if name := pkg.Name + "." + sel.Sel.Name; archivePathCalls[name] {
			hits = append(hits, fset.Position(call.Pos()).String()+": "+name)
		}
		return true
	})
	return hits
}

func TestArchiveIsOnlyTouchedThroughTheWalker(t *testing.T) {
	const dir = "../analytics"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	scanned := 0
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
		for _, hit := range findArchivePathCalls(fset, f) {
			t.Errorf("%s: archive files are deleted and enumerated only through the walker in archive_walk.go (os.Root)", hit)
		}
	}
	if scanned == 0 {
		t.Fatal("no analytics sources scanned; this guard is looking at nothing")
	}
}

// The guard must catch a violation, not just pass on clean code.
func TestArchiveWalkerGuardCatchesASeededViolation(t *testing.T) {
	src := `package analytics
import ("os"; "path/filepath")
func bad(dir string) { m, _ := filepath.Glob(dir + "/dt=*"); for _, f := range m { _ = os.Remove(f) } }`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "seeded.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hits := findArchivePathCalls(fset, f); len(hits) != 2 {
		t.Fatalf("seeded violation found %d hits, want 2: %v", len(hits), hits)
	}
}
