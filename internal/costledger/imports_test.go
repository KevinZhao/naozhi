package costledger_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageIsLeaf pins the leaf-package contract from docs/rfc/cost-ledger.md
// §4: every producer (session, cron, sysession, wireup) imports costledger,
// so costledger must import no internal/* package back.
func TestPackageIsLeaf(t *testing.T) {
	t.Parallel()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
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
			if p := strings.Trim(imp.Path.Value, `"`); strings.HasPrefix(p, forbidden) {
				t.Errorf("%s imports %q — costledger must remain a leaf package", name, p)
			}
		}
	}
}
