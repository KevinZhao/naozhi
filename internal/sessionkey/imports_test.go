package sessionkey_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageIsLeaf enforces the sessionkey leaf-package contract: no
// production source file in internal/sessionkey may import any other
// internal/* package. The .golangci.yml depguard rule encodes the same
// invariant for static lint, but CI only runs go test today — this
// keeps the rule enforced regardless.
//
// Why this matters: sessionkey exists specifically to break the
// cron → session prefix-sharing cycle (see docs/rfc/cron-sysession-merge.md
// §3.2). A future change adding `import ".../internal/session"` here
// would silently re-introduce the cycle and defeat the package.
//
// _test.go files are excluded — tests can import whatever they want.
// stdlib + third-party imports are allowed (the package itself uses
// "strings"); the rule is specifically about other internal/* packages.
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
			// imp.Path.Value is quoted — strip the quotes.
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(p, forbidden) {
				t.Errorf("%s imports %q — sessionkey must remain a leaf package", name, p)
			}
		}
	}
}
