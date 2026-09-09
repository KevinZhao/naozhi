// rule 3b-send (send_engine_ownership): the send pipeline's state belongs to
// sendEngine, not Hub, and the HTTP handler reaches the engine only through
// its methods (#2551, #2632, docs/rfc/send-engine-extraction.md §2.3).
//
// This is the send-block slice of the "rule 3b AST field_block 对账" that
// main.go has listed as owed since Phase 4b. Phase 4b was shelved by ADR-001,
// so the general rule was never going to arrive; #2551 delivers the part it can
// actually check today. Rule 3a only verifies that a marker comment EXISTS —
// its content is never validated, which is how wshub_send.go's header ended up
// claiming ownership of a field that no longer exists. These checks look at the
// declarations instead, so they cannot drift into fiction.
//
// Three checks, all AST-on-declarations (no go/types pass, no build tags):
//
//	A. Neither sendEngine nor SendHandler may declare a field of type *Hub.
//	   That is the core boundary RFC §2.1 draws — the engine and the HTTP
//	   handler depend on the send pipeline, not on the WebSocket layer — and
//	   the regression most likely to be added back "just for one call". #2551
//	   shipped this as a six-name blocklist (queue/guard/wg/...) on Hub, which
//	   a rename could walk around and which said nothing about SendHandler;
//	   #2632 replaced it with the type check.
//	B. send.go / send_owner_loop.go / send_engine.go carry *sendEngine
//	   receivers only: a *Hub method there is a piece of the pipeline written
//	   back onto the Hub.
//	C. dashboard_send.go may CALL engine methods but never READ an engine
//	   field. `h.engine.sessionSend(...)` is fine; `h.engine.allowedRoot` is
//	   not. Before #2632 the handler did the latter in 8 places, which was the
//	   old hub+router "two views of one state" pattern with a new name.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// sendPipelineFiles hold the pipeline methods. They must carry *sendEngine
// receivers only: a *Hub method here means a piece of the pipeline was written
// back onto the Hub.
var sendPipelineFiles = []string{"send.go", "send_owner_loop.go", "send_engine.go"}

// sendHandlerFile is the HTTP transport over the engine; check C reads it.
const sendHandlerFile = "dashboard_send.go"

// sendBoundaryTypes are the structs that must not hold a *Hub (check A), with
// the file each is expected to live in for the "type not found" message.
var sendBoundaryTypes = map[string]string{
	"sendEngine":  "send_engine.go",
	"SendHandler": sendHandlerFile,
}

// scanSendEngineOwnership implements rule 3b-send.
func scanSendEngineOwnership(pkgDir string) []Violation {
	var out []Violation
	fset := token.NewFileSet()

	// A: no *Hub field on the boundary structs. Scan every non-test file so a
	// moved struct is still found (and a missing one is still reported).
	found := map[string]bool{}
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return append(out, Violation{
			Rule: "send_engine_ownership", File: filepath.ToSlash(pkgDir),
			Message: fmt.Sprintf("read package dir: %v", err),
		})
	}
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(pkgDir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			out = append(out, Violation{
				Rule:    "send_engine_ownership",
				File:    filepath.ToSlash(path),
				Message: fmt.Sprintf("parse failed, cannot check send boundary: %v", err),
			})
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if _, boundary := sendBoundaryTypes[ts.Name.Name]; !boundary {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			found[ts.Name.Name] = true
			for _, fld := range st.Fields.List {
				if !isStarHub(fld.Type) {
					continue
				}
				label := "embedded *Hub"
				if len(fld.Names) > 0 {
					label = fmt.Sprintf("field %q", fld.Names[0].Name)
				}
				out = append(out, Violation{
					Rule:    "send_engine_ownership",
					File:    filepath.ToSlash(path),
					Line:    fset.Position(fld.Pos()).Line,
					Message: fmt.Sprintf("%s declares %s of type *Hub. The send pipeline and its HTTP transport depend on sendEngine, not on the WebSocket layer (#2551 §2.1, #2632); pass the specific dependency (router, resolver, notifier) instead of a Hub back-pointer", ts.Name.Name, label),
				})
			}
			return true
		})
	}
	for typ, home := range sendBoundaryTypes {
		if !found[typ] {
			out = append(out, Violation{
				Rule: "send_engine_ownership", File: filepath.ToSlash(filepath.Join(pkgDir, home)),
				Message: fmt.Sprintf("type %s not found — the send boundary lost a side (#2551); if it moved packages, move this rule with it", typ),
			})
		}
	}
	if !found["sendEngine"] {
		// Without the engine the remaining checks are about a type that does
		// not exist; say so once rather than cascading.
		return out
	}

	// B: the pipeline files must not carry *Hub methods.
	for _, name := range sendPipelineFiles {
		path := filepath.Join(pkgDir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			out = append(out, Violation{
				Rule: "send_engine_ownership", File: filepath.ToSlash(path),
				Message: fmt.Sprintf("parse failed, cannot check receivers: %v", err),
			})
			continue
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			// recvTypeName lives in main.go (shared with rule 1).
			if recvTypeName(fd.Recv.List[0].Type) != "Hub" {
				continue
			}
			out = append(out, Violation{
				Rule:    "send_engine_ownership",
				File:    filepath.ToSlash(path),
				Line:    fset.Position(fd.Pos()).Line,
				Message: fmt.Sprintf("%s has a *Hub receiver in a send-pipeline file (#2551). Pipeline methods take *sendEngine; if this really is Hub's job (a WS protocol adapter, a node-table accessor), put it in the wshub_*.go file for that job — lookupNode sitting here is what made the chain's method count read 13 instead of 12", fd.Name.Name),
			})
		}
	}

	// C: dashboard_send.go calls engine methods, never reads engine fields.
	out = append(out, scanEngineFieldReads(fset, filepath.Join(pkgDir, sendHandlerFile))...)
	return out
}

// isStarHub reports whether e is the type expression `*Hub`.
func isStarHub(e ast.Expr) bool {
	star, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "Hub"
}

// scanEngineFieldReads flags every `<x>.engine.<name>` in path that is NOT the
// callee of a call expression. A method call keeps the engine's invariants in
// the engine; a field read copies them into the handler, which is how the
// allowedRoot / resolver / ctx / notify reads accumulated before #2632.
func scanEngineFieldReads(fset *token.FileSet, path string) []Violation {
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		if os.IsNotExist(err) {
			return []Violation{{
				Rule: "send_engine_ownership", File: filepath.ToSlash(path),
				Message: "dashboard_send.go not found — the HTTP send handler moved; point check C at its new file",
			}}
		}
		return []Violation{{
			Rule: "send_engine_ownership", File: filepath.ToSlash(path),
			Message: fmt.Sprintf("parse failed, cannot check engine field reads: %v", err),
		}}
	}
	// First pass: every SelectorExpr that is a call's callee is a method call.
	callees := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				callees[sel] = true
			}
		}
		return true
	})
	var out []Violation
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || callees[sel] {
			return true
		}
		inner, ok := sel.X.(*ast.SelectorExpr)
		if !ok || inner.Sel.Name != "engine" {
			return true
		}
		out = append(out, Violation{
			Rule:    "send_engine_ownership",
			File:    filepath.ToSlash(path),
			Line:    fset.Position(sel.Pos()).Line,
			Message: fmt.Sprintf("dashboard_send.go reads engine field %q directly. The HTTP handler reaches the engine through methods only (#2632) — add one to send_engine.go (see the method surface block there) rather than copying engine state into the handler", sel.Sel.Name),
		})
		return true
	})
	return out
}
