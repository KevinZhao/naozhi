package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestRunHistoryTaskHelperPinned guards the R222-ARCH-17 (#748) helper:
// once the full HistorySubsystem extraction lands (#383), this contract
// can move alongside the new owner. Until then, runHistoryTask is the
// canonical late-Add(1)-safe spawner — its presence on Router is
// load-bearing for any future subsystem that wants history-tracked
// goroutines without re-baking the historyCtx.Err() race fix.
func TestRunHistoryTaskHelperPinned(t *testing.T) {
	// AST-parse router_core.go and assert the method is defined on
	// Router with the expected `func(ctx context.Context)` signature.
	// A literal grep would catch a rename; the AST check additionally
	// catches a signature drift (e.g. someone changing the callback to
	// no-ctx, which would silently break the historyCtx-aware adoption
	// path described in the godoc).
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "router_core.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse router_core.go: %v", err)
	}

	var found *ast.FuncDecl
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runHistoryTask" {
			continue
		}
		if fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		// Receiver must be (*Router).
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok || ident.Name != "Router" {
			continue
		}
		found = fn
		break
	}
	if found == nil {
		t.Fatal("runHistoryTask method removed from *Router. " +
			"R222-ARCH-17 (#748) requires the late-Add(1)-safe spawner " +
			"to remain available on Router until the full HistorySubsystem " +
			"split (#383) lands. If the split DID land, move this test to " +
			"the new owner package and update the contract.")
	}

	// Param: exactly one, of shape `func(ctx context.Context)`.
	if found.Type.Params == nil || len(found.Type.Params.List) != 1 {
		t.Fatalf("runHistoryTask must take exactly one param (the task fn); got %v", found.Type.Params)
	}
	paramType, ok := found.Type.Params.List[0].Type.(*ast.FuncType)
	if !ok {
		t.Fatalf("runHistoryTask param must be a func; got %T", found.Type.Params.List[0].Type)
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
	if found.Type.Results == nil || len(found.Type.Results.List) != 1 {
		t.Fatalf("runHistoryTask must return one value (bool); got %v", found.Type.Results)
	}
	retIdent, ok := found.Type.Results.List[0].Type.(*ast.Ident)
	if !ok || retIdent.Name != "bool" {
		t.Errorf("runHistoryTask return type drifted from bool; got %v", found.Type.Results.List[0].Type)
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
