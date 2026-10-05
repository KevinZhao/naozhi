// anchor-keep: the package's source facts — a leaf-package import ban the cron↔sysession merge RFC depends on, and the enum constant sets (its own, and cron's re-exports of them), which reflect cannot enumerate.
package runtelemetry

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
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
// value would put an unfrozen string on the wire, on disk and over REST. Each
// enum type must alias this package's, each top-level enum const must re-export
// one of ours, cron must not convert into an enum type, and every constant
// expression of an enum type must name a typed enum constant, so an untyped
// literal assigned to an enum field, variable or argument fails too. The zero
// value "" is exempt: a bare `var` declaration yields it without a literal.
func TestCronEnumsReexportRuntelemetry(t *testing.T) {
	t.Parallel()
	fset, files := parseProductionFiles(t, filepath.Join("..", "cron"))
	info := typeCheckCron(t, fset, files)
	aliases, reexports := 0, 0
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				if !isCronEnumConst(vs, info) {
					continue
				}
				reexports++
				if vs.Type != nil {
					t.Errorf("%s: const %s names a type; re-export the runtelemetry constant untyped", fset.Position(vs.Pos()), vs.Names[0].Name)
				}
				if len(vs.Values) != len(vs.Names) {
					t.Errorf("%s: const %s has no value of its own; re-export a runtelemetry constant", fset.Position(vs.Pos()), vs.Names[0].Name)
				}
				for _, v := range vs.Values {
					if !namesEnumConst(info, v) {
						t.Errorf("%s: const %s must name a runtelemetry constant, not a local value", fset.Position(v.Pos()), vs.Names[0].Name)
					}
				}
			}
		}
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
	var bad []string
	uses := 0
	for e, tv := range info.Types {
		if tv.Value == nil || cronEnumOf(tv.Type) == "" || constant.StringVal(tv.Value) == "" {
			continue
		}
		uses++
		if !namesEnumConst(info, e) {
			bad = append(bad, fmt.Sprintf("%s: constant %s of type %s is not a runtelemetry constant", fset.Position(e.Pos()), tv.Value, cronEnumOf(tv.Type)))
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
	if uses < 50 {
		t.Errorf("found only %d enum-typed constant expressions in cron: the type check has gone blind", uses)
	}
}

// typeCheckCron type-checks cron's files for this GOOS against this package's
// own source. Every other import fails and becomes a fake package, so types
// are known only where they need nothing beyond cron, runtelemetry and
// builtins; the resulting type errors are expected and ignored.
func typeCheckCron(t *testing.T, fset *token.FileSet, cron map[string]*ast.File) *types.Info {
	t.Helper()
	noImports := importerFunc(func(path string) (*types.Package, error) { return nil, fmt.Errorf("not loaded: %s", path) })
	ignore := func(error) {}
	ownFset, own := parseProductionFiles(t, ".")
	conf := types.Config{Importer: noImports, Error: ignore}
	rt, _ := conf.Check("github.com/naozhi/naozhi/internal/runtelemetry", ownFset, buildFiles(t, ".", own), nil)
	conf.Importer = importerFunc(func(path string) (*types.Package, error) {
		if path == rt.Path() {
			return rt, nil
		}
		return noImports(path)
	})
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	_, _ = conf.Check("github.com/naozhi/naozhi/internal/cron", fset, buildFiles(t, filepath.Join("..", "cron"), cron), info)
	return info
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// buildFiles returns the files the current build context compiles, in name
// order.
func buildFiles(t *testing.T, dir string, files map[string]*ast.File) []*ast.File {
	t.Helper()
	var names []string
	for name := range files {
		ok, err := build.Default.MatchFile(dir, name)
		if err != nil {
			t.Fatalf("match %s: %v", name, err)
		}
		if ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]*ast.File, len(names))
	for i, name := range names {
		out[i] = files[name]
	}
	return out
}

// cronEnumOf returns the name of the run enum typ is, or "".
func cronEnumOf(typ types.Type) string {
	n, ok := types.Unalias(typ).(*types.Named)
	if !ok || n.Obj().Pkg() == nil || n.Obj().Pkg().Path() != "github.com/naozhi/naozhi/internal/runtelemetry" {
		return ""
	}
	if _, ok := cronEnums[n.Obj().Name()]; !ok {
		return ""
	}
	return n.Obj().Name()
}

// namesEnumConst reports whether e is a reference to a constant declared with
// a run enum type, which cron's own const check pins to runtelemetry's.
func namesEnumConst(info *types.Info, e ast.Expr) bool {
	e = ast.Unparen(e)
	if s, ok := e.(*ast.SelectorExpr); ok {
		e = s.Sel
	}
	id, ok := e.(*ast.Ident)
	if !ok {
		return false
	}
	c, ok := info.Uses[id].(*types.Const)
	return ok && cronEnumOf(c.Type()) != ""
}

// isCronEnumConst reports whether a top-level const spec declares a run enum
// value: it is enum-typed, or a name carries an enum's constant prefix and its
// type is a string. A const whose type derives from an unloaded package is
// unknown and not counted.
func isCronEnumConst(vs *ast.ValueSpec, info *types.Info) bool {
	if _, ok := cronEnumType(vs.Type); ok {
		return true
	}
	for _, name := range vs.Names {
		obj := info.Defs[name]
		if obj == nil {
			continue
		}
		if cronEnumOf(obj.Type()) != "" {
			return true
		}
		if b, ok := obj.Type().Underlying().(*types.Basic); !ok || b.Info()&types.IsString == 0 {
			continue
		}
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
