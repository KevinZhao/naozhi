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

// defaultExemptionsPath is where main reads exemptions.yaml from unless
// -exemptions names another file.
const defaultExemptionsPath = "tools/lint-server-handlers/exemptions.yaml"

// handlerDecl is one HTTP handler declaration. Key is "Recv.name" for a
// method (pointer stripped) and the bare name for a free function or var.
type handlerDecl struct {
	Key  string
	File string
	Line int
}

// pkgFile is one parsed non-test file and the name it imports net/http by
// ("" when it does not).
type pkgFile struct {
	path string
	f    *ast.File
	typ  handlerTyper
}

// scanHandlerDecls returns pkgDir's non-test handler declarations, sorted by key:
//   - a function or method of exactly (http.ResponseWriter, *http.Request)
//     with no results (ServeHTTP included),
//   - a factory: its single result is a handler type and no parameter is one
//     (that is middleware), or
//   - a package-level var whose type is a handler type, or whose value is a
//     handler or factory func literal or a conversion to a handler type.
//
// Known gaps: func literals registered inline, a factory taking a handler it
// never wraps, and an untyped var initialised by calling a factory.
func scanHandlerDecls(pkgDir string) ([]handlerDecl, error) {
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []pkgFile
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
		files = append(files, pkgFile{path: path, f: f, typ: handlerTyper{httpName: netHTTPName(f)}})
	}
	local := localHandlerTypes(files)
	var out []handlerDecl
	for _, pf := range files {
		ht := pf.typ
		ht.local = local
		add := func(key string, pos token.Pos) {
			out = append(out, handlerDecl{Key: key, File: filepath.ToSlash(pf.path), Line: fset.Position(pos).Line})
		}
		for _, decl := range pf.f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !ht.isHandlerFunc(d.Type) && !ht.isFactory(d.Type) {
					continue
				}
				key := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					key = handlerRecvName(d.Recv.List[0].Type) + "." + key
				}
				add(key, d.Pos())
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, id := range vs.Names {
						if id.Name == "_" {
							continue
						}
						if (vs.Type != nil && ht.isHandler(vs.Type)) || (len(vs.Values) == len(vs.Names) && ht.isHandlerValue(vs.Values[i])) {
							add(id.Name, id.Pos())
						}
					}
				}
			}
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

// localHandlerTypes names the package's own types defined as a handler type
// (type fn func(http.ResponseWriter, *http.Request), type h http.HandlerFunc,
// or one defined in turn as such a type).
func localHandlerTypes(files []pkgFile) map[string]bool {
	local := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, pf := range files {
			ht := pf.typ
			ht.local = local
			for _, decl := range pf.f.Decls {
				d, ok := decl.(*ast.GenDecl)
				if !ok || d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts := spec.(*ast.TypeSpec)
					if !local[ts.Name.Name] && ts.TypeParams == nil && ht.isHandler(ts.Type) {
						local[ts.Name.Name] = true
						changed = true
					}
				}
			}
		}
	}
	return local
}

// scanHandleDecl implements rule 1 against baseline, read from baselineFile.
func scanHandleDecl(pkgDir string, baseline []string, baselineFile string) ([]Violation, error) {
	decls, err := scanHandlerDecls(pkgDir)
	if err != nil {
		return nil, err
	}
	listed := make(map[string]bool, len(baseline))
	var out []Violation
	for _, k := range baseline {
		if listed[k] {
			out = append(out, Violation{
				Rule:    "handle_decl",
				File:    baselineFile,
				Message: fmt.Sprintf("handle_baseline lists %q more than once; delete the repeat", k),
			})
		}
		listed[k] = true
	}
	found := make(map[string]bool, len(decls))
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
		found[k] = true // a repeated stale entry is reported once
		out = append(out, Violation{
			Rule:    "handle_decl",
			File:    baselineFile,
			Message: fmt.Sprintf("handle_baseline entry %q matches no HTTP handler declaration in %s; delete the entry", k, filepath.ToSlash(pkgDir)),
		})
	}
	return out, nil
}

// netHTTPName is the name f refers to net/http by: "http", an alias, or "."
// for a dot import; "" when f does not import it usably.
func netHTTPName(f *ast.File) string {
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err != nil || p != "net/http" {
			continue
		}
		if imp.Name == nil {
			return "http"
		}
		if imp.Name.Name == "_" {
			return ""
		}
		return imp.Name.Name
	}
	return ""
}

// handlerTyper classifies the type expressions of one file: httpName is the
// file's name for net/http ("" when not imported), local the package's own
// handler types (localHandlerTypes).
type handlerTyper struct {
	httpName string
	local    map[string]bool
}

// isHTTPType reports whether e names net/http's type `name`.
func (h handlerTyper) isHTTPType(e ast.Expr, name string) bool {
	switch h.httpName {
	case "":
		return false
	case ".":
		id, ok := e.(*ast.Ident)
		return ok && id.Name == name
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == h.httpName
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

// isHandlerFunc: (http.ResponseWriter, *http.Request) with no results.
func (h handlerTyper) isHandlerFunc(ft *ast.FuncType) bool {
	params := fieldTypes(ft.Params)
	if len(params) != 2 || len(fieldTypes(ft.Results)) != 0 {
		return false
	}
	star, ok := params[1].(*ast.StarExpr)
	return ok && h.isHTTPType(params[0], "ResponseWriter") && h.isHTTPType(star.X, "Request")
}

// isHandler: http.Handler, http.HandlerFunc, a handler-shaped func type, or a
// local type defined as one of those.
func (h handlerTyper) isHandler(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.FuncType:
		return h.isHandlerFunc(t)
	case *ast.Ident:
		if h.local[t.Name] {
			return true
		}
	}
	return h.isHTTPType(e, "Handler") || h.isHTTPType(e, "HandlerFunc")
}

// isFactory: returns exactly one handler and takes none.
func (h handlerTyper) isFactory(ft *ast.FuncType) bool {
	results := fieldTypes(ft.Results)
	if len(results) != 1 || !h.isHandler(results[0]) {
		return false
	}
	for _, p := range fieldTypes(ft.Params) {
		if h.isHandler(p) {
			return false
		}
	}
	return true
}

// isHandlerValue: a handler or factory func literal, or a conversion to a
// handler type (http.HandlerFunc(fn)).
func (h handlerTyper) isHandlerValue(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.FuncLit:
		return h.isHandlerFunc(v.Type) || h.isFactory(v.Type)
	case *ast.CallExpr:
		return len(v.Args) == 1 && h.isHandler(v.Fun)
	}
	return false
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
