// anchor-keep: the package's source facts — a leaf-package import ban the cron↔sysession merge RFC depends on, and the enum constant set, which reflect cannot enumerate.
package runtelemetry

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
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

	const forbidden = "github.com/naozhi/naozhi/internal/"
	_, files := parseProductionFiles(t)
	for name, f := range files {
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(p, forbidden) {
				t.Errorf("%s imports %q — runtelemetry must remain a leaf package", name, p)
			}
		}
	}
}

// parseProductionFiles parses every non-test .go file of the package, keyed by
// file name.
func parseProductionFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %q: %v", dir, err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatalf("no production .go files under %s", dir)
	}
	return fset, files
}

// declaredEnums returns, for every `type X string` the package declares, the
// wire literal of each constant of that type. A constant the scan cannot read
// as `Name X = "literal"` fails the test instead of being skipped, since a
// skipped constant is one the wire freeze never sees.
func declaredEnums(t *testing.T) map[string][]string {
	t.Helper()
	fset, files := parseProductionFiles(t)
	enums := map[string][]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
				for _, s := range gd.Specs {
					if ts := s.(*ast.TypeSpec); identName(ts.Type) == "string" {
						enums[ts.Name.Name] = []string{}
					}
				}
			}
		}
	}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			prevEnum := false // an untyped spec with no values repeats the previous one
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				typ := identName(vs.Type)
				if enums[typ] == nil {
					if vs.Type == nil && len(vs.Values) == 0 && prevEnum {
						t.Fatalf("%s: const %s repeats the previous enum spec; spell out its type and literal",
							fset.Position(vs.Pos()), vs.Names[0].Name)
					}
					for _, v := range vs.Values {
						if c, ok := v.(*ast.CallExpr); ok && enums[identName(c.Fun)] != nil {
							t.Fatalf("%s: const %s converts to %s; declare it as `Name %s = \"...\"` so the enum freeze sees it",
								fset.Position(vs.Pos()), vs.Names[0].Name, identName(c.Fun), identName(c.Fun))
						}
					}
					prevEnum = false
					continue
				}
				prevEnum = true
				if len(vs.Values) != len(vs.Names) {
					t.Fatalf("%s: const %s has %d values for %d names", fset.Position(vs.Pos()), vs.Names[0].Name, len(vs.Values), len(vs.Names))
				}
				for i, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: const %s is not a string literal", fset.Position(v.Pos()), vs.Names[i].Name)
					}
					val, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: unquote %s: %v", fset.Position(v.Pos()), lit.Value, err)
					}
					enums[typ] = append(enums[typ], val)
				}
			}
		}
	}
	return enums
}

func identName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
