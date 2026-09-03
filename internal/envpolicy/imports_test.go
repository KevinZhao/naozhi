package envpolicy_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageIsLeaf enforces the envpolicy leaf-package contract: no
// production source file in internal/envpolicy may import any other
// internal/* package.
//
// envpolicy is imported by cmd/naozhi, internal/sysession, internal/shim and
// internal/node (#891 Phase 1, #2300 Phase 2). Any of those importing back
// into envpolicy — or envpolicy reaching into any internal/* — would create
// an import cycle and break the "single leaf classifier" property the SSRF
// guards rely on. _test.go files are excluded; stdlib and third-party
// imports are fine.
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
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(p, forbidden) {
				t.Errorf("%s imports %q — envpolicy must remain a leaf package", name, p)
			}
		}
	}
}
