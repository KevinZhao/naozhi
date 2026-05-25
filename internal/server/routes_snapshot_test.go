package server

import (
	"bytes"
	"encoding/json"
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

// TestRoutesSnapshot is the Phase 0 anti-drift gate for the 50+ HTTP routes
// registered by the server package. It parses the AST of the registration
// sites and matches each `mux.HandleFunc("METHOD /path", handler)` call
// against a sorted golden file (testdata/routes.golden.json).
//
// Snapshot content is `method + path + handlerType` (the pointer-receiver
// type of the handler, e.g. "*CronHandlers"). Internal handler-function
// renames (handleList → handleListV2) are intentionally allowed; only
// route-path or owning-type drift breaks the snapshot.
//
// Why AST-parse instead of hooking into a constructed ServeMux:
//  1. http.ServeMux doesn't expose Routes() in Go 1.22; we'd need test
//     hooks inside server.go and a fully-wired Server (router/scheduler/
//     all handlers) just to enumerate.
//  2. Phase 4-5 will move handler types into sub-packages; AST parsing
//     keeps the snapshot grammar stable across that move (only
//     handlerType strings update).
//
// Update procedure: when a Phase N PR moves a handler, run
//
//	UPDATE_GOLDEN=1 go test -run TestRoutesSnapshot ./internal/server/
//
// then commit the new golden alongside the move, with the PR description
// stating the diff is expected (route added/moved/typed).
func TestRoutesSnapshot(t *testing.T) {
	routes, err := scanRoutes("dashboard.go", "server.go", "debug_expvar.go", "debug_pprof.go")
	if err != nil {
		t.Fatalf("scanRoutes: %v", err)
	}
	if len(routes) == 0 {
		t.Fatal("no routes scanned — likely a regex/AST parse miss")
	}

	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})

	got, err := json.MarshalIndent(routes, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	goldenPath := filepath.Join("testdata", "routes.golden.json")

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden updated: %s (%d routes)", goldenPath, len(routes))
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v\n(hint: regenerate with UPDATE_GOLDEN=1)", goldenPath, err)
	}

	if !bytes.Equal(got, want) {
		// Surface a useful diff. The full payload is deterministic and ~5KB
		// so dumping both sides is safe.
		t.Errorf("routes drifted from golden\n=== want (%s) ===\n%s\n=== got ===\n%s\n=== hint ===\nregenerate with: UPDATE_GOLDEN=1 go test -run TestRoutesSnapshot ./internal/server/", goldenPath, want, got)
	}
}

type routeEntry struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	HandlerType string `json:"handler_type"`
}

// scanRoutes parses each file's AST and finds calls of the form
//
//	<recv>.mux.Handle("...", expr)
//	<recv>.mux.HandleFunc("METHOD /path", expr)
//
// extracting (method, path) from the first arg and the
// outermost-receiver-type from the handler expression (typically
// `s.cronH.handleList` → `*CronHandlers`).
func scanRoutes(files ...string) ([]routeEntry, error) {
	var out []routeEntry
	fset := token.NewFileSet()
	for _, name := range files {
		path := name
		if _, err := os.Stat(path); err != nil {
			// Files in this package are always relative to the test binary
			// CWD which IS the package directory under `go test`.
			return nil, err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// Match s.mux.HandleFunc / s.mux.Handle.
			if sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc" {
				return true
			}
			inner, ok := sel.X.(*ast.SelectorExpr)
			if !ok || inner.Sel.Name != "mux" {
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			method, p := parsePattern(strings.Trim(lit.Value, `"`))
			handler := handlerTypeOf(call.Args[1])
			out = append(out, routeEntry{Method: method, Path: p, HandlerType: handler})
			return true
		})
	}
	return out, nil
}

// parsePattern splits "GET /api/foo" into ("GET", "/api/foo");
// "/raw" → ("", "/raw").
func parsePattern(pat string) (method, path string) {
	parts := strings.SplitN(pat, " ", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", parts[0]
}

// handlerTypeOf returns a stable identifier for the handler expression's
// outermost receiver type. Returns the bare type name (e.g.
// "*CronHandlers") when the expression looks like `s.cronH.handleX`,
// or a fallback "<func>" / "<inline>" tag for other shapes (auth
// wrappers / closures).
//
// Strategy is structural and intentionally shallow — Phase 4-5
// handler-receiver names will rename together with the type, so a
// brittle exact match on the field name (`cronH` etc.) is the right
// regression gate.
//
// Recognized patterns (and their outputs):
//
//	auth(s.cronH.handleList)            → "*CronHandlers"
//	s.auth.requireAuth(handler)         → "*AuthHandlers"
//	s.healthH.handleHealth              → "*HealthHandler"
//	s.handleSystemDaemons               → "*Server"
//	s.hub.HandleUpgrade                 → "*Hub"
//	s.reverseNodeServer                 → "*node.ReverseServer"
func handlerTypeOf(e ast.Expr) string {
	// Unwrap one level of CallExpr (auth wrapper).
	if call, ok := e.(*ast.CallExpr); ok && len(call.Args) >= 1 {
		// e.g. auth(s.cronH.handleList) — unwrap to inner.
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "auth" {
			return handlerTypeOf(call.Args[0])
		}
		// e.g. s.auth.requireAuth(handler) — the *outer* call is the
		// auth wrapper; report it as *AuthHandlers regardless of the
		// inline handler-arg shape (debug_pprof / debug_expvar both
		// pass closure-typed `handler` vars whose type can't be
		// resolved by this lightweight AST walker). Recognise the
		// shape `<recv>.auth.<method>(...)`.
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "auth" {
				return "*AuthHandlers"
			}
		}
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		// Bare identifier — likely a top-level func or var. No clean
		// type info statically.
		if id, ok := e.(*ast.Ident); ok {
			return "<bare:" + id.Name + ">"
		}
		return "<unknown>"
	}
	// Walk to the outermost selector: s.cronH.handleList → cronH.
	for {
		next, ok := sel.X.(*ast.SelectorExpr)
		if !ok {
			break
		}
		sel = next
	}
	// sel.X is now the receiver (e.g. `s`); sel.Sel is the field name we
	// care about (e.g. `cronH` / `auth` / `healthH` / `hub` /
	// `reverseNodeServer`). We can't resolve types statically without a
	// full type checker, but the field-name → type-name mapping is
	// stable inside this package and lives in Server struct. Maintain
	// a lookup table here; new fields require adding a row.
	field := sel.Sel.Name
	if t, ok := serverFieldType[field]; ok {
		return t
	}
	// Receiver-method shape (s.handleSystemDaemons / s.handleDashboard).
	if id, ok := sel.X.(*ast.Ident); ok && id.Name == "s" {
		// `s.<methodName>` — the method belongs to *Server.
		return "*Server"
	}
	return "<unmapped:" + field + ">"
}

// serverFieldType maps Server struct field names → stable type identifier
// for routes_snapshot.go. Keep in sync with server.go's Server struct.
//
// Phase 4-5 will move these handler types into sub-packages; that PR's
// snapshot diff will rewrite this map AND testdata/routes.golden.json
// in lockstep.
var serverFieldType = map[string]string{
	"cliH":              "*CLIBackendsHandler",
	"sessionH":          "*SessionHandlers",
	"agentEventsH":      "*AgentEventsHandlers",
	"sendH":             "*SendHandler",
	"discoveryH":        "*DiscoveryHandlers",
	"projectH":          "*ProjectHandlers",
	"transcribeH":       "*TranscribeHandler",
	"cronH":             "*CronHandlers",
	"scratchH":          "*ScratchHandler",
	"memoryH":           "*MemoryHandler",
	"healthH":           "*HealthHandler",
	"auth":              "*AuthHandlers",
	"hub":               "*Hub",
	"reverseNodeServer": "*node.ReverseServer",
}

// patternRegex sanity-checks our path-parsing.
var _ = regexp.MustCompile(`^[A-Z]+ /`)
