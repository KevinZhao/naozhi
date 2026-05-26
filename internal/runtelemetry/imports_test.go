package runtelemetry_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageIsLeaf enforces the runtelemetry leaf-package contract: no
// production source file in internal/runtelemetry may import any other
// internal/* package. Both producers (cron / sysession) and the
// hubBroadcaster implementation (server) depend on this — pulling any
// internal back here would tangle the graph the package is meant to
// straighten.
//
// _test.go is excluded.
func TestPackageIsLeaf(t *testing.T) {
	t.Parallel()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %q: %v", dir, err)
	}

	const forbidden = "github.com/naozhi/naozhi/internal/"
	fset := token.NewFileSet()

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(p, forbidden) {
				t.Errorf("%s imports %q — runtelemetry must remain a leaf package", name, p)
			}
		}
	}
}
