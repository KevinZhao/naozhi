// anchor-keep: the encapsulation is a property of where code lives — which file may name tbl's fields — so only the source can show it; the fixtures below prove each rule fires.
package cron

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// jobTableViolations checks the jobTable boundary (#2959) over a package's
// non-test files, keyed by base name:
//
//   - field: nothing names a jobTable field through tbl (the table's own
//     methods reach them through their receiver).
//   - locked: nothing calls a tbl method ending in Locked.
//   - funcparam: no jobTable method takes a function, which could run
//     arbitrary code under the lock.
//   - lock: a jobTable method not ending in Locked that touches its mutex opens
//     with Lock/RLock and a deferred matching unlock, and touches it nowhere
//     else; a Locked method never takes the mutex.
func jobTableViolations(fset *token.FileSet, files map[string]*ast.File) []string {
	fields := map[string]bool{}
	funcTypes := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if _, isFunc := ts.Type.(*ast.FuncType); isFunc {
				funcTypes[ts.Name.Name] = true
			}
			if st, isStruct := ts.Type.(*ast.StructType); isStruct && ts.Name.Name == "jobTable" {
				for _, fl := range st.Fields.List {
					for _, name := range fl.Names {
						fields[name.Name] = true
					}
				}
			}
			return true
		})
	}

	var out []string
	report := func(rule string, pos token.Pos, format string, args ...any) {
		out = append(out, fmt.Sprintf("%s: %s: %s", rule, fset.Position(pos), fmt.Sprintf(format, args...)))
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		f := files[name]
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.SelectorExpr); ok && x.Sel.Name == "tbl" {
				switch {
				case fields[sel.Sel.Name]:
					report("field", sel.Pos(), "tbl.%s reaches past the table's API", sel.Sel.Name)
				case strings.HasSuffix(sel.Sel.Name, "Locked"):
					report("locked", sel.Pos(), "tbl.%s needs a lock only the table holds", sel.Sel.Name)
				}
			}
			return true
		})
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 || recvTypeName(fd.Recv.List[0].Type) != "jobTable" {
				continue
			}
			for _, p := range fd.Type.Params.List {
				if isFuncType(p.Type, funcTypes) {
					report("funcparam", p.Pos(), "jobTable.%s takes a function", fd.Name.Name)
				}
			}
			checkLockShape(fd, report)
		}
	}
	return out
}

func recvTypeName(e ast.Expr) string {
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func isFuncType(e ast.Expr, named map[string]bool) bool {
	switch t := e.(type) {
	case *ast.FuncType:
		return true
	case *ast.Ident:
		return named[t.Name]
	}
	return false
}

// muCall returns the method name of a recv.mu.<M>() call statement, or "".
func muCall(e ast.Expr, recv string) string {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	mu, ok := sel.X.(*ast.SelectorExpr)
	if !ok || mu.Sel.Name != "mu" {
		return ""
	}
	if id, ok := mu.X.(*ast.Ident); ok && id.Name == recv {
		return sel.Sel.Name
	}
	return ""
}

func checkLockShape(fd *ast.FuncDecl, report func(string, token.Pos, string, ...any)) {
	if fd.Body == nil || len(fd.Recv.List[0].Names) == 0 {
		return
	}
	recv := fd.Recv.List[0].Names[0].Name
	var uses []*ast.SelectorExpr
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "mu" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
				uses = append(uses, sel)
			}
		}
		return true
	})
	if strings.HasSuffix(fd.Name.Name, "Locked") {
		if len(uses) > 0 {
			report("lock", uses[0].Pos(), "jobTable.%s is Locked but takes the mutex", fd.Name.Name)
		}
		return
	}
	if len(uses) == 0 {
		return
	}
	pair := map[string]string{"Lock": "Unlock", "RLock": "RUnlock"}
	stmts := fd.Body.List
	ok := len(stmts) >= 2 && len(uses) == 2
	if ok {
		es, isExpr := stmts[0].(*ast.ExprStmt)
		ds, isDefer := stmts[1].(*ast.DeferStmt)
		ok = isExpr && isDefer
		if ok {
			unlock, isLock := pair[muCall(es.X, recv)]
			ok = isLock && muCall(ds.Call, recv) == unlock
		}
	}
	if !ok {
		report("lock", fd.Pos(), "jobTable.%s must open with %s.mu.Lock/RLock and a deferred unlock, and touch mu nowhere else", fd.Name.Name, recv)
	}
}

func parseCronSources(t *testing.T, srcs map[string]string) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for name, src := range srcs {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
	}
	return fset, files
}

func TestJobTableEncapsulation(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	srcs := map[string]string{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		srcs[p] = string(b)
	}
	if _, ok := srcs["jobtable.go"]; !ok {
		t.Fatal("jobtable.go not found; run from internal/cron")
	}
	fset, files := parseCronSources(t, srcs)
	for _, v := range jobTableViolations(fset, files) {
		t.Error(v)
	}
}

// Each rule fires on the shape it forbids, so a gate that silently stopped
// matching would fail here rather than pass everything.
func TestJobTableEncapsulation_CatchesEachRule(t *testing.T) {
	const table = `package cron
import "sync"
type hookFn func()
type jobTable struct { mu sync.RWMutex; jobs map[string]int }
func (t *jobTable) good(id string) int { t.mu.RLock(); defer t.mu.RUnlock(); return t.jobs[id] }
func (t *jobTable) goodLocked() int { return len(t.jobs) }
func (t *jobTable) pure() int { return 1 }
`
	for _, tc := range []struct {
		name, file, src, rule string
	}{
		{"field read", "scheduler.go", `package cron
type Scheduler struct{ tbl jobTable }
func (s *Scheduler) f() int { return len(s.tbl.jobs) }`, "field"},
		{"mutex", "scheduler.go", `package cron
type Scheduler struct{ tbl jobTable }
func (s *Scheduler) f() { s.tbl.mu.Lock() }`, "field"},
		{"locked call", "scheduler.go", `package cron
type Scheduler struct{ tbl jobTable }
func (s *Scheduler) f() int { return s.tbl.goodLocked() }`, "locked"},
		{"func literal param", "jobtable_x.go", `package cron
func (t *jobTable) with(fn func()) { fn() }`, "funcparam"},
		{"named func param", "jobtable_x.go", `package cron
func (t *jobTable) with(fn hookFn) { fn() }`, "funcparam"},
		{"no defer", "jobtable_x.go", `package cron
func (t *jobTable) bad() { t.mu.Lock(); t.mu.Unlock() }`, "lock"},
		{"mismatched unlock", "jobtable_x.go", `package cron
func (t *jobTable) bad() { t.mu.RLock(); defer t.mu.Unlock() }`, "lock"},
		{"lock not first", "jobtable_x.go", `package cron
func (t *jobTable) bad() int { n := 1; t.mu.Lock(); defer t.mu.Unlock(); return n }`, "lock"},
		{"second lock", "jobtable_x.go", `package cron
func (t *jobTable) bad() { t.mu.Lock(); defer t.mu.Unlock(); t.mu.Unlock() }`, "lock"},
		{"unlock first", "jobtable_x.go", `package cron
func (t *jobTable) bad() { t.mu.Unlock(); defer println(&t.mu) }`, "lock"},
		{"field in a jobtable file", "jobtable_x.go", `package cron
type Scheduler struct{ tbl jobTable }
func (s *Scheduler) f() int { return len(s.tbl.jobs) }`, "field"},
		{"Locked takes the lock", "jobtable_x.go", `package cron
func (t *jobTable) badLocked() { t.mu.Lock(); defer t.mu.Unlock() }`, "lock"},
	} {
		fset, files := parseCronSources(t, map[string]string{"jobtable.go": table, tc.file: tc.src})
		got := jobTableViolations(fset, files)
		if len(got) != 1 || !strings.HasPrefix(got[0], tc.rule+": ") {
			t.Errorf("%s: violations = %q, want exactly one %q", tc.name, got, tc.rule)
		}
	}
	fset, files := parseCronSources(t, map[string]string{"jobtable.go": table, "scheduler.go": `package cron
type Scheduler struct{ tbl jobTable }
func (s *Scheduler) f() int { return s.tbl.good("x") + s.tbl.pure() }`})
	if got := jobTableViolations(fset, files); len(got) != 0 {
		t.Errorf("the allowed shapes were flagged: %q", got)
	}
}

// testTableLockBaseline is how many times cron tests take the registry lock
// through tblForTest: the lock-order tests, which hold or probe it to prove
// the discipline, and one identity check that needs the live pointer.
// Everything else seeds and reads through the export_test.go ports (#3007).
const testTableLockBaseline = 29

// Tests reach the registry lock only where the lock itself is under test.
// More is a regression to a port; fewer means the baseline comes down with it.
func TestTableLockRatchet(t *testing.T) {
	paths, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	needle := "tblForTest" + "().mu"
	n := 0
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		n += strings.Count(string(b), needle)
	}
	switch {
	case n > testTableLockBaseline:
		t.Errorf("cron tests take the registry lock through tblForTest %d times, above the baseline of %d: seed and read through the export_test.go ports", n, testTableLockBaseline)
	case n < testTableLockBaseline:
		t.Errorf("cron tests take the registry lock through tblForTest %d times: lower testTableLockBaseline to %d", n, n)
	}
}
