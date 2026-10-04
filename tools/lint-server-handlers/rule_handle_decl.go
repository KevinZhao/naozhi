// rule 1 (handle_decl): every HTTP handler declared in the server package is
// listed in exemptions.yaml handle_baseline, and every entry there still names
// one.
//
// A handler is recognised by its signature, not its name or receiver: a
// renamed method or one moved to another receiver type is still a handler, and
// still something internal/server/doc.go says the package should not grow.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// defaultExemptionsPath is where main reads exemptions.yaml from; a stale
// handle_baseline entry is reported against it.
const defaultExemptionsPath = "tools/lint-server-handlers/exemptions.yaml"

// handlerDecl is one HTTP handler declaration. Key is "Recv.name" for a
// method (pointer stripped) and the bare name for a free function.
type handlerDecl struct {
	Key  string
	File string
	Line int
}

// scanHandlerDecls returns every handler declaration in pkgDir's non-test Go
// files, sorted by key. A declaration is a handler when it is either
//   - a function or method of exactly (http.ResponseWriter, *http.Request)
//     with no results (ServeHTTP included), or
//   - a factory: its single result is http.Handler, http.HandlerFunc or a
//     handler-shaped func type, and no parameter is one (that is middleware).
//
// Func literals registered inline (mux.HandleFunc("/x", func(w, r) {...}))
// are expressions, not declarations, and are out of scope.
func scanHandlerDecls(pkgDir string) ([]handlerDecl, error) {
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var out []handlerDecl
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		path := filepath.Join(pkgDir, n)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		httpName, ok := netHTTPName(f)
		if !ok {
			continue
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || !(isHandlerFuncType(fd.Type, httpName) || isHandlerFactory(fd.Type, httpName)) {
				continue
			}
			key := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				key = handlerRecvName(fd.Recv.List[0].Type) + "." + key
			}
			out = append(out, handlerDecl{Key: key, File: filepath.ToSlash(path), Line: fset.Position(fd.Pos()).Line})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].File < out[j].File
	})
	return out, nil
}

// scanHandleDecl implements rule 1 against baseline.
func scanHandleDecl(pkgDir string, baseline []string) ([]Violation, error) {
	decls, err := scanHandlerDecls(pkgDir)
	if err != nil {
		return nil, err
	}
	listed := make(map[string]bool, len(baseline))
	for _, k := range baseline {
		listed[k] = true
	}
	found := make(map[string]bool, len(decls))
	var out []Violation
	for _, d := range decls {
		found[d.Key] = true
		if listed[d.Key] {
			continue
		}
		out = append(out, Violation{
			Rule:    "handle_decl",
			File:    d.File,
			Line:    d.Line,
			Message: fmt.Sprintf("%s is an HTTP handler declared in the server package, which owns only the HTTP pipe; every other /api/* handler lives in an internal/dashboard/<sub> package behind a Deps struct (internal/server/doc.go). Move it, or list it in exemptions.yaml handle_baseline if it is part of the pipe", d.Key),
		})
	}
	for _, k := range baseline {
		if found[k] {
			continue
		}
		out = append(out, Violation{
			Rule:    "handle_decl",
			File:    defaultExemptionsPath,
			Message: fmt.Sprintf("handle_baseline entry %q matches no HTTP handler declaration in %s; delete the entry", k, filepath.ToSlash(pkgDir)),
		})
	}
	return out, nil
}

// handlerKeys is the deduplicated key list -gen-baseline records.
func handlerKeys(decls []handlerDecl) []string {
	var out []string
	for _, d := range decls {
		if len(out) == 0 || out[len(out)-1] != d.Key {
			out = append(out, d.Key)
		}
	}
	return out
}

// netHTTPName is the name f refers to net/http by: "http", an alias, or "."
// for a dot import. ok is false when f does not import it usably.
func netHTTPName(f *ast.File) (string, bool) {
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err != nil || p != "net/http" {
			continue
		}
		if imp.Name == nil {
			return "http", true
		}
		if imp.Name.Name == "_" {
			return "", false
		}
		return imp.Name.Name, true
	}
	return "", false
}

// isHTTPType reports whether e names net/http's type `name`.
func isHTTPType(e ast.Expr, httpName, name string) bool {
	if httpName == "." {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == name
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == httpName
}

// fieldTypes flattens a field list to one type per parameter or result.
func fieldTypes(fl *ast.FieldList) []ast.Expr {
	if fl == nil {
		return nil
	}
	var out []ast.Expr
	for _, f := range fl.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for range n {
			out = append(out, f.Type)
		}
	}
	return out
}

// isHandlerFuncType: (http.ResponseWriter, *http.Request) with no results.
func isHandlerFuncType(ft *ast.FuncType, httpName string) bool {
	params := fieldTypes(ft.Params)
	if len(params) != 2 || len(fieldTypes(ft.Results)) != 0 {
		return false
	}
	star, ok := params[1].(*ast.StarExpr)
	return ok && isHTTPType(params[0], httpName, "ResponseWriter") && isHTTPType(star.X, httpName, "Request")
}

// isHandlerType: http.Handler, http.HandlerFunc or a handler-shaped func type.
func isHandlerType(e ast.Expr, httpName string) bool {
	if ft, ok := e.(*ast.FuncType); ok {
		return isHandlerFuncType(ft, httpName)
	}
	return isHTTPType(e, httpName, "Handler") || isHTTPType(e, httpName, "HandlerFunc")
}

// isHandlerFactory: returns exactly one handler and takes none.
func isHandlerFactory(ft *ast.FuncType, httpName string) bool {
	results := fieldTypes(ft.Results)
	if len(results) != 1 || !isHandlerType(results[0], httpName) {
		return false
	}
	for _, p := range fieldTypes(ft.Params) {
		if isHandlerType(p, httpName) {
			return false
		}
	}
	return true
}

// handlerRecvName is the receiver's type name with pointer and type
// parameters stripped.
func handlerRecvName(e ast.Expr) string {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	switch t := e.(type) {
	case *ast.IndexExpr:
		e = t.X
	case *ast.IndexListExpr:
		e = t.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
