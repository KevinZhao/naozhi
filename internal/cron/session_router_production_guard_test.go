package cron

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestSessionRouterProductionGuardPresent pins that the
// `var _ SessionRouter = (*session.Router)(nil)` assertion lives in
// scheduler_session.go (a non-test file) so signature drift surfaces on
// `go build` not just `go test`. R249-ARCH-7 (#973).
//
// The session-package contract_test.go ALSO pins this for the cross-
// consumer fan-in, but that file is in package session_test and does not
// participate in `go build ./internal/cron/...`. A future cleanup that
// "consolidates" the consumer asserts into the test-only file would lose
// the build-time guarantee — this test catches that regression by
// asserting the production guard is still parseable in a non-_test.go
// file.
func TestSessionRouterProductionGuardPresent(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "scheduler_session.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse scheduler_session.go: %v", err)
	}
	found := false
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// Match a blank-identifier var with type SessionRouter and
			// initialiser referencing session.Router.
			if len(vs.Names) != 1 || vs.Names[0].Name != "_" {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "SessionRouter" {
				continue
			}
			if len(vs.Values) != 1 {
				continue
			}
			// Render the initialiser to text and check for session.Router.
			text := exprText(vs.Values[0])
			if strings.Contains(text, "session.Router") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("scheduler_session.go missing `var _ SessionRouter = (*session.Router)(nil)` guard; signature drift would no longer surface on `go build` (R249-ARCH-7 / #973)")
	}
}

// exprText renders an ast.Expr to a flat dotted/star/paren textual form
// without round-tripping through go/printer (which would pull a heavy
// dependency). Sufficient for the initialiser shape we expect:
//   - StarExpr → "*X"
//   - ParenExpr → "(X)"
//   - SelectorExpr → "X.Y"
//   - Ident → "X"
//   - CallExpr → "F(args)" (only used to flatten the (*session.Router)(nil)
//     conversion form).
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return "*" + exprText(v.X)
	case *ast.ParenExpr:
		return "(" + exprText(v.X) + ")"
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		args := make([]string, 0, len(v.Args))
		for _, a := range v.Args {
			args = append(args, exprText(a))
		}
		return exprText(v.Fun) + "(" + strings.Join(args, ", ") + ")"
	}
	return ""
}
