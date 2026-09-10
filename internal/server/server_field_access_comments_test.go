package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestServerFieldAccessComments_MatchSources is the Server-struct twin of
// tools/check-router-fields (#2637). Every field of `type Server struct` in
// server.go carries a `// 读写: <files>` (or `// 读: <files>`) trailer naming
// the non-test files that touch it. Those trailers were rewritten by hand in
// #2617 and had drifted again by #2619 — a file that stopped touching s.mux
// was still listed, a field read from build_dashboard.go claimed routes.go,
// send_dispatch_adapter.go's router read was missing — because nothing
// compared the list with the code. This test does: it walks every non-test
// .go file in the package, collects `<recv>.<field>` selectors inside
// *Server methods (plus `c.s.<field>` in serverCaps and the struct literal in
// buildServer), and fails on any file that is accessed-but-unlisted or
// listed-but-unused.
//
// The trailer grammar is deliberately narrow: file tokens are `name.go`;
// anything in parentheses is prose and ignored.
func TestServerFieldAccessComments_MatchSources(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	declared := parseServerFieldTrailers(t, fset)
	if len(declared) < 20 {
		t.Fatalf("parsed only %d annotated Server fields — struct or trailer grammar drift", len(declared))
	}
	actual := scanServerFieldAccess(t, fset, declared)

	var names []string
	for n := range declared {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, f := range names {
		want, got := declared[f], actual[f]
		for file := range got {
			if !want[file] {
				t.Errorf("Server.%s is accessed in %s but its 读写 trailer does not list it", f, file)
			}
		}
		for file := range want {
			if !got[file] {
				t.Errorf("Server.%s trailer lists %s, which no longer touches the field", f, file)
			}
		}
	}
}

var (
	trailerRe = regexp.MustCompile(`(?:读写|读):\s*(.*)$`)
	goFileRe  = regexp.MustCompile(`\b[a-z0-9_]+\.go\b`)
	parenRe   = regexp.MustCompile(`\([^)]*\)`)
)

// parseServerFieldTrailers returns field → declared file set for every Server
// field that has a trailer. Fields without one are reported as failures so a
// new field cannot opt out silently.
func parseServerFieldTrailers(t *testing.T, fset *token.FileSet) map[string]map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(fset, "server.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Server" {
			return true
		}
		st := ts.Type.(*ast.StructType)
		for _, fld := range st.Fields.List {
			if len(fld.Names) == 0 {
				continue
			}
			name := fld.Names[0].Name
			text := ""
			if fld.Comment != nil {
				text = fld.Comment.Text()
			}
			if fld.Doc != nil {
				text += "\n" + fld.Doc.Text()
			}
			var files map[string]bool
			for _, line := range strings.Split(text, "\n") {
				m := trailerRe.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				files = map[string]bool{}
				for _, tok := range goFileRe.FindAllString(parenRe.ReplaceAllString(m[1], ""), -1) {
					files[tok] = true
				}
			}
			if files == nil {
				t.Errorf("Server.%s has no `// 读写:` trailer", name)
				continue
			}
			out[name] = files
		}
		return false
	})
	return out
}

// scanServerFieldAccess returns field → set of non-test files that access it
// through a *Server receiver, through serverCaps' `c.s.<field>`, or in the
// Server struct literal.
func scanServerFieldAccess(t *testing.T, fset *token.FileSet, fields map[string]map[string]bool) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	mark := func(field, file string) {
		if _, ok := fields[field]; !ok {
			return
		}
		if out[field] == nil {
			out[field] = map[string]bool{}
		}
		out[field][file] = true
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			// Identifiers that denote a *Server in this function: the method
			// receiver, plus locals assigned from `&Server{...}` or from the
			// constructors (buildServerWithHandlers binds `s` that way).
			serverIdents := map[string]bool{}
			if fd.Recv != nil && len(fd.Recv.List) == 1 && len(fd.Recv.List[0].Names) == 1 {
				if star, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
					if id, ok := star.X.(*ast.Ident); ok && id.Name == "Server" {
						serverIdents[fd.Recv.List[0].Names[0].Name] = true
					}
				}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) == 0 || len(as.Rhs) != 1 {
					return true
				}
				lhs, ok := as.Lhs[0].(*ast.Ident)
				if !ok {
					return true
				}
				switch r := as.Rhs[0].(type) {
				case *ast.UnaryExpr:
					if cl, ok := r.X.(*ast.CompositeLit); ok {
						if id, ok := cl.Type.(*ast.Ident); ok && id.Name == "Server" {
							serverIdents[lhs.Name] = true
						}
					}
				case *ast.CallExpr:
					if id, ok := r.Fun.(*ast.Ident); ok && strings.HasPrefix(id.Name, "buildServer") {
						serverIdents[lhs.Name] = true
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					switch base := x.X.(type) {
					case *ast.Ident:
						if serverIdents[base.Name] {
							mark(x.Sel.Name, name)
						}
					case *ast.SelectorExpr:
						// serverCaps{s *Server}: c.s.<field>
						if base.Sel.Name == "s" {
							if id, ok := base.X.(*ast.Ident); ok && id.Name == "c" {
								mark(x.Sel.Name, name)
							}
						}
					}
				case *ast.CompositeLit:
					// &Server{ field: ... } in buildServerWithHandlers.
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "Server" {
						for _, el := range x.Elts {
							if kv, ok := el.(*ast.KeyValueExpr); ok {
								if k, ok := kv.Key.(*ast.Ident); ok {
									mark(k.Name, name)
								}
							}
						}
					}
				}
				return true
			})
		}
	}
	return out
}
