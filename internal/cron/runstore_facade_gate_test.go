package cron

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runStoreFacadeFile is the single cron source file allowed to touch
// s.runStore.<method> directly: it defines the *Scheduler facade wrappers
// (runStoreEnabled / appendRun / recentSessionIDs / trimAllRuns /
// deleteJobRuns) plus the read-side query methods (ListRuns / RecentRuns /
// GetRun). Every other cron file must route through those wrappers. Keep this
// in sync with where the wrappers live.
const runStoreFacadeFile = "scheduler_finish.go"

// TestNoDirectRunStoreAccess pins the runStore half-facade (#509): no cron
// production source file other than the wrapper-definition file may reference
// s.runStore.<field/method> directly. New write/lifecycle access must go
// through a *Scheduler wrapper so the runStore's surface (and its independent
// lock hierarchy) stays reachable from exactly one file — the prerequisite for
// the deferred Phase-2 sub-package extraction.
//
// The check is AST-based: it walks every SelectorExpr and flags
// `<recv>.runStore.<x>` where <recv> is the receiver identifier of the
// enclosing method (typically `s`). Matching the receiver name (rather than a
// hardcoded "s") keeps the gate correct if a future method renames its
// receiver. _test.go files and the facade file itself are exempt.
func TestNoDirectRunStoreAccess(t *testing.T) {
	t.Parallel()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %q: %v", dir, err)
	}

	fset := token.NewFileSet()
	violations := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == runStoreFacadeFile {
			continue
		}
		path := filepath.Join(dir, name)
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil {
				return true
			}
			recv := receiverName(fn.Recv.List[0])
			if recv == "" {
				return true
			}
			ast.Inspect(fn.Body, func(bn ast.Node) bool {
				sel, ok := bn.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				// Match <recv>.runStore (the inner selector). The outer
				// SelectorExpr <recv>.runStore.<x> nests this as sel.X, so
				// inspecting every SelectorExpr catches the inner node once.
				inner, ok := sel.X.(*ast.Ident)
				if !ok || inner.Name != recv || sel.Sel.Name != "runStore" {
					return true
				}
				pos := fset.Position(sel.Pos())
				t.Errorf("%s:%d: direct %s.runStore access in method with receiver %q — route through a *Scheduler facade wrapper (see %s)",
					name, pos.Line, recv, recv, runStoreFacadeFile)
				violations++
				return true
			})
			return true
		})
	}
	if violations == 0 {
		t.Logf("runStore facade intact: no direct s.runStore access outside %s", runStoreFacadeFile)
	}
}

// receiverName returns the identifier bound to a method receiver field, or ""
// for an unnamed receiver (e.g. `func (*Scheduler) M()`).
func receiverName(field *ast.Field) string {
	if len(field.Names) == 0 {
		return ""
	}
	return field.Names[0].Name
}
