// anchor-keep: the package's source facts — a leaf-package import ban the cron↔sysession merge RFC depends on, and the enum constant sets (its own, and cron's re-exports of them), which reflect cannot enumerate.
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
	_, files := parseProductionFiles(t, ".")
	for name, f := range files {
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(p, forbidden) {
				t.Errorf("%s imports %q — runtelemetry must remain a leaf package", name, p)
			}
		}
	}
}

// parseProductionFiles parses every non-test .go file in dir, keyed by file
// name.
func parseProductionFiles(t *testing.T, dir string) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
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
// wire literal of each constant of that type. An enum-typed constant must read
// `Name X = "literal"`, and any other const whose value mentions an enum type
// or constant fails the test, so no const spelling yields a wire value the
// freeze never sees. Runtime conversions are not constants and stay out of
// scope.
func declaredEnums(t *testing.T) map[string][]string {
	t.Helper()
	fset, files := parseProductionFiles(t, ".")
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
	enumConsts := map[string]bool{}
	var others []*ast.ValueSpec
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			gd, ok := n.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				return true
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
					others = append(others, vs)
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
					enumConsts[vs.Names[i].Name] = true
					enums[typ] = append(enums[typ], val)
				}
			}
			return false
		})
	}
	for _, vs := range others {
		for _, v := range vs.Values {
			ast.Inspect(v, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && (enums[id.Name] != nil || enumConsts[id.Name]) {
					t.Fatalf("%s: const %s derives from %s; declare it as `Name T = \"...\"` so the enum freeze sees it",
						fset.Position(vs.Pos()), vs.Names[0].Name, id.Name)
				}
				return true
			})
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

// cronEnums maps each run enum type cron aliases to its constants' name prefix.
var cronEnums = map[string]string{"ErrorClass": "ErrClass", "RunState": "RunState", "TriggerKind": "Trigger"}

// TestCronEnumsReexportRuntelemetry pins that cron mints no run enum value of
// its own: the wire freeze sees only this package's constants, so a cron-local
// type, literal or conversion would put an unfrozen string on the wire, on
// disk and over REST. Each enum type must alias this package's, each constant
// with an enum prefix must re-export one of ours, and cron's production code
// must not convert into an enum type.
func TestCronEnumsReexportRuntelemetry(t *testing.T) {
	t.Parallel()
	fset, files := parseProductionFiles(t, filepath.Join("..", "cron"))
	aliases, reexports := 0, 0
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.TypeSpec:
				if _, ok := cronEnums[n.Name.Name]; !ok {
					return true
				}
				aliases++
				if n.Assign == 0 || !isRuntelemetrySel(n.Type, n.Name.Name) {
					t.Errorf("%s: type %s must be `= runtelemetry.%s`", fset.Position(n.Pos()), n.Name.Name, n.Name.Name)
				}
			case *ast.ValueSpec:
				if !isCronEnumConst(n) {
					return true
				}
				reexports++
				if n.Type != nil {
					t.Errorf("%s: const %s names a type; re-export the runtelemetry constant untyped", fset.Position(n.Pos()), n.Names[0].Name)
				}
				if len(n.Values) != len(n.Names) {
					t.Errorf("%s: const %s has no value of its own; re-export a runtelemetry constant", fset.Position(n.Pos()), n.Names[0].Name)
				}
				for _, v := range n.Values {
					if !isRuntelemetrySel(v, "") {
						t.Errorf("%s: const %s must be a runtelemetry constant, not a local value", fset.Position(v.Pos()), n.Names[0].Name)
					}
				}
			case *ast.CallExpr:
				if name, ok := cronEnumType(n.Fun); ok {
					t.Errorf("%s: conversion to %s; use a runtelemetry constant", fset.Position(n.Pos()), name)
				}
			}
			return true
		})
	}
	if aliases != len(cronEnums) {
		t.Errorf("found %d cron run enum type specs, want %d: the scan has gone blind or a type moved", aliases, len(cronEnums))
	}
	if reexports < 20 {
		t.Errorf("found only %d cron run enum constants: the scan has gone blind", reexports)
	}
}

// isCronEnumConst reports whether a value spec declares a run enum value: it
// names an enum type, or one of its names carries an enum's constant prefix.
func isCronEnumConst(vs *ast.ValueSpec) bool {
	if _, ok := cronEnumType(vs.Type); ok {
		return true
	}
	for _, name := range vs.Names {
		for _, prefix := range cronEnums {
			if strings.HasPrefix(name.Name, prefix) {
				return true
			}
		}
	}
	return false
}

// cronEnumType reports whether e names a run enum type, bare or qualified by
// runtelemetry.
func cronEnumType(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.Ident:
		_, ok := cronEnums[e.Name]
		return e.Name, ok
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok && x.Name == "runtelemetry" {
			_, ok := cronEnums[e.Sel.Name]
			return "runtelemetry." + e.Sel.Name, ok
		}
	}
	return "", false
}

// isRuntelemetrySel reports whether e is runtelemetry.<sel>, any sel if empty.
func isRuntelemetrySel(e ast.Expr, sel string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := s.X.(*ast.Ident)
	return ok && x.Name == "runtelemetry" && (sel == "" || s.Sel.Name == sel)
}
