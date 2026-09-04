// rule 6 (api_route_owner): every `s.mux.Handle*("… /api/…", h)` registration
// in routes.go must hand the request to a dashboard sub-package handler, never
// to a *Server method. This is the decidable form of the ownership rule in
// internal/server/doc.go; rule 1 covers declarations, this covers wiring.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// scanAPIRouteOwner parses path (routes.go) and applies scanAPIRouteOwnerFile.
func scanAPIRouteOwner(path string) ([]Violation, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return scanAPIRouteOwnerFile(fset, f), nil
}

// scanAPIRouteOwnerFile walks every method on *Server / Server in f and
// reports each `<recv>.mux.Handle` / `HandleFunc` call whose pattern contains
// "/api/" and whose handler argument, after unwrapping call wrappers such as
// `auth(...)`, is `<recv>.handleX` / `<recv>.HandleX`.
func scanAPIRouteOwnerFile(fset *token.FileSet, f *ast.File) []Violation {
	var out []Violation
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 || fd.Body == nil {
			continue
		}
		if recvTypeName(fd.Recv.List[0].Type) != "Server" {
			continue
		}
		recv := ""
		if names := fd.Recv.List[0].Names; len(names) == 1 {
			recv = names[0].Name
		}
		if recv == "" || recv == "_" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isMuxRegistration(call, recv) || len(call.Args) < 2 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !strings.Contains(lit.Value, "/api/") {
				return true
			}
			for _, method := range serverHandlerRefs(call.Args[1], recv) {
				pos := fset.Position(call.Pos())
				out = append(out, Violation{
					Rule: "api_route_owner",
					File: pos.Filename,
					Line: pos.Line,
					Message: fmt.Sprintf("%s registers %s on Server method %q; /api/* handlers live in an internal/dashboard/<sub> package and are wired via a Deps struct (internal/server/doc.go)",
						call.Fun.(*ast.SelectorExpr).Sel.Name, lit.Value, method),
				})
			}
			return true
		})
	}
	return out
}

// isMuxRegistration reports whether call is `<recv>.mux.Handle(...)` or
// `<recv>.mux.HandleFunc(...)`.
func isMuxRegistration(call *ast.CallExpr, recv string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
		return false
	}
	mux, ok := sel.X.(*ast.SelectorExpr)
	if !ok || mux.Sel.Name != "mux" {
		return false
	}
	id, ok := mux.X.(*ast.Ident)
	return ok && id.Name == recv
}

// serverHandlerRefs returns the `<recv>.handleX` method names reachable from
// e by walking through CallExpr arguments (wrappers like auth(h), or nested
// auth(wrap(h))). Selectors through a field (`<recv>.systemH.HandleX`) are
// dashboard handlers and are not reported.
func serverHandlerRefs(e ast.Expr, recv string) []string {
	switch x := e.(type) {
	case *ast.CallExpr:
		var out []string
		for _, arg := range x.Args {
			out = append(out, serverHandlerRefs(arg, recv)...)
		}
		return out
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok || id.Name != recv {
			return nil
		}
		if strings.HasPrefix(x.Sel.Name, "handle") || strings.HasPrefix(x.Sel.Name, "Handle") {
			return []string{"Server." + x.Sel.Name}
		}
	}
	return nil
}
