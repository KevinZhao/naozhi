// anchor-keep: runHistoryTask is the canonical late-Add(1)-safe spawner; a bare `go` bypass compiles and only loses WaitGroup accounting during Stop.
package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunHistoryTaskHelperPinned guards the R222-ARCH-17 (#748) helper:
// runHistoryTask is the canonical late-Add(1)-safe spawner, and since S12d
// (#3023) it lives on HistoryIO, the facet that owns the history ctx and wait
// group. Its presence there is load-bearing for any caller that wants
// history-tracked goroutines without re-baking the ctx.Err() race fix.
func TestRunHistoryTaskHelperPinned(t *testing.T) {
	// AST-parse every non-test file of the package and assert the method is
	// defined on *HistoryIO, exactly once, with the expected
	// `func(ctx context.Context)` signature. A literal grep would catch a
	// rename; the AST check additionally catches a signature drift (e.g.
	// someone changing the callback to no-ctx, which would silently break the
	// ctx-aware adoption path described in the godoc) and does not depend on
	// which file holds it.
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var found []*ast.FuncDecl
	parsed := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		parsed++
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "runHistoryTask" || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			// Receiver must be (*HistoryIO).
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if ident, ok := star.X.(*ast.Ident); ok && ident.Name == "HistoryIO" {
				found = append(found, fn)
			}
		}
	}
	if parsed < 40 {
		t.Fatalf("parsed %d non-test files, want at least 40: the scan has gone blind", parsed)
	}
	if len(found) != 1 {
		t.Fatalf("found %d runHistoryTask methods on *HistoryIO, want 1. "+
			"R222-ARCH-17 (#748) requires the late-Add(1)-safe spawner "+
			"to stay on the facet that owns the history ctx and wait group.", len(found))
	}
	fn := found[0]

	// Param: exactly one, of shape `func(ctx context.Context)`.
	if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		t.Fatalf("runHistoryTask must take exactly one param (the task fn); got %v", fn.Type.Params)
	}
	paramType, ok := fn.Type.Params.List[0].Type.(*ast.FuncType)
	if !ok {
		t.Fatalf("runHistoryTask param must be a func; got %T", fn.Type.Params.List[0].Type)
	}
	if paramType.Params == nil || len(paramType.Params.List) != 1 {
		t.Fatalf("task fn must take exactly one ctx arg; got %v", paramType.Params)
	}
	// The ctx arg type must reference context.Context. Print and check substring —
	// covers both `context.Context` and dot-imported variants.
	var sb strings.Builder
	if err := printerExpr(&sb, paramType.Params.List[0].Type); err != nil {
		t.Fatalf("print ctx arg type: %v", err)
	}
	if got := sb.String(); !strings.Contains(got, "Context") {
		t.Errorf("task fn ctx arg type = %q, want something containing Context", got)
	}

	// Return type: bool (refuse-on-cancel signal). A signature change to
	// `func ... ` (no return) would silently strip the late-Add(1)
	// observability and let callers assume success.
	if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
		t.Fatalf("runHistoryTask must return one value (bool); got %v", fn.Type.Results)
	}
	retIdent, ok := fn.Type.Results.List[0].Type.(*ast.Ident)
	if !ok || retIdent.Name != "bool" {
		t.Errorf("runHistoryTask return type drifted from bool; got %v", fn.Type.Results.List[0].Type)
	}
}

// printerExpr is a tiny helper that prints a Go AST expression to the
// builder without pulling in go/printer's full file/Pos machinery — we
// only need a substring check on the ctx arg's type identifier.
func printerExpr(sb *strings.Builder, e ast.Expr) error {
	switch v := e.(type) {
	case *ast.Ident:
		sb.WriteString(v.Name)
	case *ast.SelectorExpr:
		if err := printerExpr(sb, v.X); err != nil {
			return err
		}
		sb.WriteByte('.')
		sb.WriteString(v.Sel.Name)
	case *ast.StarExpr:
		sb.WriteByte('*')
		return printerExpr(sb, v.X)
	default:
		// Unknown shape — print the type name of the AST node so the
		// failure message is at least informative.
		sb.WriteString("<")
		sb.WriteString(strings.TrimPrefix(
			strings.SplitN(strings.TrimPrefix(
				typeName(e), "*"), " ", 2)[0],
			"ast."))
		sb.WriteString(">")
	}
	return nil
}

func typeName(v any) string {
	if v == nil {
		return "<nil>"
	}
	return strings.TrimPrefix(
		// Use Sprintf %T via fmt would pull fmt; do a minimal reflect-free
		// fallback instead. The common AST shapes we care about are caught
		// by the explicit cases above; this branch only fires on unknown
		// shapes where any string is fine.
		"unknown", "*",
	)
}
