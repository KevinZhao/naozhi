// anchor-keep: storage-boundary gate: no cron sandbox file may do disk IO of its own; a bypassing call compiles fine and silently skips the guards sandboxstore encodes.
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
