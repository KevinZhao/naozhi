package cron

// TestCronSlogIDSanitize_Contract locks R171023-SEC-2: every slog call that
// carries a user-supplied job ID must wrap the value in
// osutil.SanitizeForLog(id, cronpkg.MaxIDLen).  This is a grep-based contract
// test — it fails the build if a new raw-id attribute is introduced without
// the wrapper, mirroring the pattern used in connector_sanitize_contract_test.go.
//
// The test works by compiling a static-analysis assertion: it searches the
// source of handlers.go and update.go for any slog call that has `"id", ` but
// does NOT immediately follow it with `osutil.SanitizeForLog`.  A match means
// a raw ID slipped through.
//
// We use go/parser on the source files at test-time rather than a runtime
// grep so the test is hermetic (no shell dependency) and produces a clear
// file:line error.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCronSlogIDSanitize_Contract(t *testing.T) {
	t.Parallel()

	// Locate the package source directory relative to this test file.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	pkgDir := filepath.Dir(thisFile)

	targets := []string{
		filepath.Join(pkgDir, "handlers.go"),
		filepath.Join(pkgDir, "update.go"),
	}

	fset := token.NewFileSet()
	for _, src := range targets {
		f, err := parser.ParseFile(fset, src, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// Only interested in slog.* calls.
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "slog" {
				return true
			}

			// Walk the argument list looking for a string literal "id"
			// immediately followed by a bare expression (not a call to
			// osutil.SanitizeForLog).
			args := call.Args
			for i := 0; i+1 < len(args); i++ {
				lit, ok := args[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				// Strip quotes.
				key := strings.Trim(lit.Value, `"`)
				if key != "id" {
					continue
				}
				// Next arg must be a call to osutil.SanitizeForLog.
				if !isSanitizeForLogCall(args[i+1]) {
					pos := fset.Position(args[i+1].Pos())
					t.Errorf("%s: slog attr \"id\" value is not wrapped in osutil.SanitizeForLog (R171023-SEC-2)", pos)
				}
			}
			return true
		})
	}
}

// isSanitizeForLogCall returns true when expr is a call of the form
// osutil.SanitizeForLog(<anything>, <anything>).
func isSanitizeForLogCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return pkg.Name == "osutil" && sel.Sel.Name == "SanitizeForLog"
}
