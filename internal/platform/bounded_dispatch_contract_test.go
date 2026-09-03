package platform_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// adapterPackages are the platform adapter directories (relative to
// internal/platform) that must consume BoundedDispatch instead of
// hand-rolling the inbound handler skeleton.
var adapterPackages = []string{"discord", "feishu", "slack", "weixin"}

// forbiddenIdent matches the identifiers each adapter used to declare for its
// private copy of the skeleton: the semaphore channel, the handler WaitGroup,
// the websocket-local semaphore, and the per-package cap constant.
var forbiddenIdent = regexp.MustCompile(`(?i)^(hookSem|handlerWg|msgSem|\w*HookConcurrency)$`)

// TestAdapters_UseBoundedDispatch is the ratchet for #2254: no platform
// adapter may re-declare the inbound handler skeleton (hookSem + handlerWg +
// recover + drop-warn + cap constant) that BoundedDispatch now owns. Before
// this helper each adapter kept a copy held in parity by "mirrors feishu"
// comments alone, and #1947 / #2009 showed a single adapter drifting.
//
// Per adapter package (production files only):
//   - no identifier (field, var, const, param) matching forbiddenIdent;
//   - no `make(chan struct{}, N)` semaphore construction;
//   - no composite literal platform.BoundedDispatch{...} setting Cap — the
//     cap is the single shared DefaultHandlerConcurrency;
//   - at least one reference to platform.BoundedDispatch (positive check so
//     an adapter that silently drops the helper is caught too).
//
// platform.RecoverHandler is unexported since #2254, so a direct call cannot
// compile; that path needs no AST check.
func TestAdapters_UseBoundedDispatch(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	for _, pkg := range adapterPackages {
		pkg := pkg
		t.Run(pkg, func(t *testing.T) {
			t.Parallel()
			entries, err := os.ReadDir(pkg)
			if err != nil {
				t.Fatalf("read dir %q: %v", pkg, err)
			}
			usesDispatch := false
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
					continue
				}
				path := filepath.Join(pkg, name)
				f, err := parser.ParseFile(fset, path, nil, 0)
				if err != nil {
					t.Fatalf("parse %s: %v", path, err)
				}
				ast.Inspect(f, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.Ident:
						if forbiddenIdent.MatchString(x.Name) {
							t.Errorf("%s: identifier %q re-declares the handler skeleton; use platform.BoundedDispatch", fset.Position(x.Pos()), x.Name)
						}
					case *ast.SelectorExpr:
						if id, ok := x.X.(*ast.Ident); ok && id.Name == "platform" && x.Sel.Name == "BoundedDispatch" {
							usesDispatch = true
						}
					case *ast.CallExpr:
						if isMakeChanStruct(x) {
							t.Errorf("%s: make(chan struct{}, N) semaphore in adapter; use platform.BoundedDispatch", fset.Position(x.Pos()))
						}
					case *ast.CompositeLit:
						if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "BoundedDispatch" {
							for _, el := range x.Elts {
								kv, ok := el.(*ast.KeyValueExpr)
								if !ok {
									continue
								}
								if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Cap" {
									t.Errorf("%s: adapter sets BoundedDispatch.Cap; all adapters share platform.DefaultHandlerConcurrency", fset.Position(kv.Pos()))
								}
							}
						}
					}
					return true
				})
			}
			if !usesDispatch {
				t.Errorf("package %s never references platform.BoundedDispatch", pkg)
			}
		})
	}
}

// isMakeChanStruct reports whether call is make(chan struct{}, ...) with a
// buffer argument — the shape of a hand-rolled semaphore.
func isMakeChanStruct(call *ast.CallExpr) bool {
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || fn.Name != "make" || len(call.Args) < 2 {
		return false
	}
	ch, ok := call.Args[0].(*ast.ChanType)
	if !ok {
		return false
	}
	st, ok := ch.Value.(*ast.StructType)
	return ok && len(st.Fields.List) == 0
}
