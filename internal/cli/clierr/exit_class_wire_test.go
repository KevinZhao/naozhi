package clierr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// Every ExitClass has its own non-empty wire name, and a class added to the
// const block without one fails here rather than reaching the dashboard as
// "unknown".
func TestExitClassWire(t *testing.T) {
	t.Parallel()
	f, err := parser.ParseFile(token.NewFileSet(), "clierr.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var classes []string
	if g := classBlock(f); g != nil {
		for _, spec := range g.Specs {
			for _, n := range spec.(*ast.ValueSpec).Names {
				classes = append(classes, n.Name)
			}
		}
	}
	wires := AllExitClassWires()
	if len(classes) == 0 || len(wires) != len(classes) {
		t.Fatalf("ExitClass consts %v, wire names %q: want one name per class", classes, wires)
	}
	for i, w := range wires {
		if w == "" || slices.Index(wires, w) != i {
			t.Errorf("wire name %d (%s) = %q: want a distinct non-empty name", i, classes[i], w)
		}
		if got := ExitClass(i).Wire(); got != w {
			t.Errorf("ExitClass(%d).Wire() = %q, want %q", i, got, w)
		}
	}
	for _, c := range []ExitClass{-1, ExitClass(len(wires))} {
		if got := c.Wire(); got != "unknown" {
			t.Errorf("ExitClass(%d).Wire() = %q, want unknown", c, got)
		}
	}
}

// classBlock is the const block declaring ExitUnknown.
func classBlock(f *ast.File) *ast.GenDecl {
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
			for _, spec := range g.Specs {
				if slices.ContainsFunc(spec.(*ast.ValueSpec).Names, func(n *ast.Ident) bool { return n.Name == "ExitUnknown" }) {
					return g
				}
			}
		}
	}
	return nil
}
