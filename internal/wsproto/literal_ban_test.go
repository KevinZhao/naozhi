package wsproto_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoServerMsgConstructionOutsideWsproto pins the #2535 contract: browser
// WS frames are built through wsproto's New* constructors only. Any
// ServerMsg / ClientMsg composite literal carrying a Type: field in
// internal/server or internal/node non-test code is a regression — the
// legacy union types survive strictly as decode-side views.
// node.ReverseMsg (node RPC, a different protocol) and clievent.EventEntry
// (event kinds, not WS types) are out of scope by design.
func TestNoServerMsgConstructionOutsideWsproto(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	for _, dir := range []string{"internal/server", "internal/node"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(root, dir, name)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if typeName(cl.Type) != "ServerMsg" && typeName(cl.Type) != "ClientMsg" {
					return true
				}
				for _, elt := range cl.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Type" {
						if _, isLit := kv.Value.(*ast.BasicLit); isLit {
							violations = append(violations,
								fset.Position(cl.Pos()).String()+": "+typeName(cl.Type)+" literal with a bare Type string — use a wsproto.New* constructor (or a wsproto constant for ClientMsg)")
						}
					}
				}
				return true
			})
		}
	}
	if len(violations) > 0 {
		t.Errorf("bare WS type literals (baseline 0):\n  %s", strings.Join(violations, "\n  "))
	}
}

func typeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}
