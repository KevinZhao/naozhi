// Rule concrete_router (#3431): internal/server's runtime code holds the
// session router through the consumer interfaces in consumer.go (HubRouter,
// serverRouter, healthRouter, …), so it can be driven by a fake and a Router
// change reaches only the wiring. A `session.Router` type reference may appear
// only in the wiring files, where the concrete pointer is handed to each
// consumer: server_options.go (the construction input), handler_set.go (the
// construction-only wiring struct), build_*.go and *_adapter.go. Test files
// and comments are not scanned.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const sessionPkgPath = "github.com/naozhi/naozhi/internal/session"

// concreteRouterAllowed reports whether a server-package file is a wiring
// file that may name the concrete router.
func concreteRouterAllowed(name string) bool {
	switch {
	case name == "server_options.go", name == "handler_set.go":
		return true
	case strings.HasPrefix(name, "build_"), strings.HasSuffix(name, "_adapter.go"):
		return true
	}
	return false
}

func scanConcreteRouter(pkgDir string) []Violation {
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return []Violation{{Rule: "concrete_router", File: pkgDir, Message: err.Error()}}
	}
	var out []Violation
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || concreteRouterAllowed(n) {
			continue
		}
		path := filepath.Join(pkgDir, n)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			out = append(out, Violation{Rule: "concrete_router", File: filepath.ToSlash(path), Message: err.Error()})
			continue
		}
		local := ""
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == sessionPkgPath {
				local = "session"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "" || local == "_" {
			continue
		}
		ast.Inspect(f, func(node ast.Node) bool {
			sel, ok := node.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Router" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
				out = append(out, Violation{Rule: "concrete_router", File: filepath.ToSlash(path), Line: fset.Position(sel.Pos()).Line,
					Message: "runtime code names *session.Router: depend on a consumer interface (consumer.go) and let the build step hand over the concrete router"})
			}
			return true
		})
	}
	return out
}
