// anchor-keep: storage-boundary gates: only the facade file may touch s.runStore directly, and no cron sandbox file may do disk IO of its own; a bypassing call compiles fine and silently skips the guards the owner encodes.
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

// diskIOCalls are the calls that read, write or remove files.
var diskIOCalls = map[string]bool{
	"os.ReadDir": true, "os.ReadFile": true, "os.Remove": true, "os.RemoveAll": true,
	"os.Open": true, "os.OpenFile": true, "os.WriteFile": true, "os.Rename": true,
	"os.Create": true, "os.CreateTemp": true, "os.Mkdir": true, "os.MkdirAll": true,
	"osutil.WriteFileAtomic": true, "jsonfile.Load": true,
}

// TestSandboxDiskIOLivesInStore pins #2897 C2: the sandbox state on disk —
// pending records, the confirmation queue, snapshots, event logs — is read and
// written by internal/cron/sandboxstore, which bounds reads, refuses symlinks
// and guards the directories. A cron sandbox*.go file doing its own IO skips
// all of that.
func TestSandboxDiskIOLivesInStore(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	var violations []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "sandbox") || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		violations = append(violations, diskIOIn(t, name)...)
	}
	if checked < 3 {
		t.Fatalf("checked %d sandbox*.go files; the scan has gone blind", checked)
	}
	for _, v := range violations {
		t.Errorf("%s: disk IO outside sandboxstore; add a Store method instead", v)
	}
}

// diskIOIn lists "file:line call" for each diskIOCalls call in file.
func diskIOIn(t *testing.T, file string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := c.Fun.(type) {
		case *ast.SelectorExpr:
			if id, ok := fn.X.(*ast.Ident); ok && diskIOCalls[id.Name+"."+fn.Sel.Name] {
				out = append(out, fset.Position(c.Pos()).String()+" "+id.Name+"."+fn.Sel.Name)
			}
		case *ast.IndexExpr: // jsonfile.Load[T](...)
			if sel, ok := fn.X.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && diskIOCalls[id.Name+"."+sel.Sel.Name] {
					out = append(out, fset.Position(c.Pos()).String()+" "+id.Name+"."+sel.Sel.Name)
				}
			}
		}
		return true
	})
	return out
}

// The detector sees each form of call, generic ones included.
func TestDiskIOIn_SeesEachForm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := "package p\n\nfunc f() {\n\tos.ReadFile(\"x\")\n\tosutil.WriteFileAtomic(\"x\", nil, 0)\n\tjsonfile.Load[int](\"x\", jsonfile.Options{})\n\tos.Getenv(\"X\")\n}\n"
	path := filepath.Join(dir, "sandbox_x.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := diskIOIn(t, path); len(got) != 3 {
		t.Errorf("found %d calls, want 3 (os.Getenv is not IO): %v", len(got), got)
	}
}
