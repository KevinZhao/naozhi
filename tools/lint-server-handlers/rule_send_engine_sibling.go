// rule send_engine_sibling (#2897 S5): sendEngine is a sibling the
// composition root builds, not a thing Hub owns and lends out. D7 picked "a
// real sibling" over "Hub + an accessor" specifically because an accessor
// re-creates the Hub → engine → Hub cycle one call deep; this rule has to
// catch every shape that call can take, not just the field read #2551/#2632
// already covered (rule_send_engine.go). Eight checks, all AST-on-declarations
// over non-test files in pkgDir:
//
//	C1 ctor point: newSendEngine(, newWSBroadcaster( and
//	   newSubscriberRegistry( may only be called from a function named
//	   buildWSStack; buildWSStack( may only be called from buildDashboard.
//	C2 engine reach: a `.engine` selector's base must be a *Hub/*SendHandler
//	   method receiver, a `w *wiring` parameter named w, or `hs.wiring`.
//	C3 accessor: no FuncDecl or FuncLit (other than newSendEngine) may return
//	   *sendEngine.
//	C4 holder whitelist: a struct field of type *sendEngine must be one of
//	   Hub.engine, SendHandler.engine, wiring.engine, HubOptions.Engine.
//	C5 Hub is not a notifier: *Hub must not declare BroadcastSessionReady,
//	   BroadcastSessionsUpdate, broadcastState or broadcastSendError.
//	C6 notifier does not point back: any type declaring broadcastState or
//	   broadcastSendError must not hold a Hub/*Hub field (named or embedded).
//	C7 no field reads: a *Hub method may call h.engine.X() / h.bcast.X() but
//	   not read a field off either.
//	C8 construction does not reach into Hub: a build*/New*/new* function may
//	   not read `<x>.hub.bcast`.
//
// C1, C2, C5 and C7 are ratchets (their own *Baseline constant, both
// directions checked); C3, C4, C6 and C8 are strict zero — nothing earns them
// a baseline because D7 already settled that no instance of them is correct.
// Each baseline constant's line carries no trailing comment on purpose:
// tools/ratchet-raises/metrics.go's goBaselineConst regex stops matching the
// moment one is added, which would make a raise invisible to the ledger
// check (#2897 S5 risk 5) — the reasoning for a value lives in the comment
// ABOVE its const line instead.
//
// Residual gap (documented, not caught): this is AST-only, so a shadowed `w`
// (`w := s.hub` inside a function that also takes `w *wiring`), an adapter
// that reaches sendNotifier through a different interface, or a closure that
// captures `hub` under a new name all slip past these checks. Catching them
// needs a go/types pass or review; the mutation table below is what AST can
// prove, not an exhaustive list of ways to rebuild the cycle.
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

// siblingCtorBaseline is the count of newSendEngine( / newWSBroadcaster( /
// newSubscriberRegistry( / buildWSStack( call sites outside their one allowed
// caller. Zero since buildWSStack became the
// engine's only constructor (#2897 S5c2); the constant stays as a strict check.
const siblingCtorBaseline = 0

// siblingEngineReachBaseline is the count of `.engine` selectors whose base
// is not an allowed one. Zero: the build steps take the engine from wiring
// and NewHub sets it in the Hub literal (#2897 S5c2). The constant stays as a
// strict check.
const siblingEngineReachBaseline = 0

// siblingHubNotifierBaseline is the count of sendNotifier-shaped methods
// still declared on *Hub. Zero since the producers bind to the broadcaster
// itself (#2897 S5c2); the constant stays as a strict check.
const siblingHubNotifierBaseline = 0

// siblingFieldReadBaseline is the count of direct `h.engine.<field>` /
// `h.bcast.<field>` reads inside *Hub methods, as opposed to method calls.
// Zero: Hub methods call the engine and the broadcaster, they never read
// their fields. The constant stays as a strict check.
const siblingFieldReadBaseline = 0

// sendEngineHolders are the only (ownerType, fieldName) pairs allowed to
// declare a *sendEngine field (C4). HubOptions.Engine is the hand-off from
// buildWSStack into NewHub, which copies it into Hub.engine.
var sendEngineHolders = map[string]map[string]bool{
	"Hub":         {"engine": true},
	"SendHandler": {"engine": true},
	"wiring":      {"engine": true},
	"HubOptions":  {"Engine": true},
}

// hubNotifierMethods are the sendNotifier-shaped names *Hub must not declare
// (C5): the full set a type needs to satisfy sendNotifier, so a Hub that
// still has all four is still usable as one, cycle intact.
var hubNotifierMethods = map[string]bool{
	"BroadcastSessionReady":   true,
	"BroadcastSessionsUpdate": true,
	"broadcastState":          true,
	"broadcastSendError":      true,
}

// notifierBackpointerMethods are the two hubNotifierMethods names specific
// enough that nothing else should ever declare them (C6): a type offering
// either is standing in for the notifier, so holding a Hub back-pointer would
// recreate the cycle one hop later.
var notifierBackpointerMethods = map[string]bool{
	"broadcastState":     true,
	"broadcastSendError": true,
}

type siblingSrcFile struct {
	path string
	f    *ast.File
}

// parseSiblingPkgDir parses every non-test .go file in pkgDir once, for the
// eight scanSibling* checks to share. Factored out of scanSendEngineSibling
// so the tests that exercise one check at a time (rule_send_engine_sibling_test.go)
// can build the same []siblingSrcFile from an in-memory fixture without this
// package's test files carrying their own go/parser read of disk — that
// pattern is exactly what the source-anchor ratchet (#2716,
// internal/testhelper/source_anchor_ratchet_test.go) exists to push back on.
func parseSiblingPkgDir(pkgDir string) (*token.FileSet, []siblingSrcFile, error) {
	fset := token.NewFileSet()
	var files []siblingSrcFile
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		path := filepath.Join(pkgDir, n)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		files = append(files, siblingSrcFile{path, f})
	}
	return fset, files, nil
}

// scanSendEngineSibling implements rule send_engine_sibling.
func scanSendEngineSibling(pkgDir string) []Violation {
	fset, files, err := parseSiblingPkgDir(pkgDir)
	if err != nil {
		return []Violation{{Rule: "send_engine_sibling", File: filepath.ToSlash(pkgDir), Message: err.Error()}}
	}

	var out []Violation
	out = append(out, ratchetViolation("send_engine_sibling", "siblingCtorBaseline", siblingCtorBaseline, scanSiblingCtorPoints(fset, files), pkgDir)...)
	out = append(out, ratchetViolation("send_engine_sibling", "siblingEngineReachBaseline", siblingEngineReachBaseline, scanSiblingEngineReach(fset, files), pkgDir)...)
	out = append(out, scanSiblingAccessors(fset, files)...)
	out = append(out, scanSiblingHolderWhitelist(fset, files)...)
	out = append(out, ratchetViolation("send_engine_sibling", "siblingHubNotifierBaseline", siblingHubNotifierBaseline, scanSiblingHubNotifiers(fset, files), pkgDir)...)
	out = append(out, scanSiblingNotifierBackpointer(fset, files)...)
	out = append(out, ratchetViolation("send_engine_sibling", "siblingFieldReadBaseline", siblingFieldReadBaseline, scanSiblingFieldReads(fset, files), pkgDir)...)
	out = append(out, scanSiblingBuildStepBcast(fset, files)...)
	return out
}

// ratchetViolation turns a found-occurrences count into the (at most one)
// violation a two-direction ratchet produces: too many is growth, too few
// means the constant was not lowered with the code.
func ratchetViolation(rule, constName string, baseline int, found []Violation, pkgDir string) []Violation {
	n := len(found)
	switch {
	case n > baseline:
		return []Violation{{Rule: rule, File: filepath.ToSlash(pkgDir),
			Message: fmt.Sprintf("%s: %d occurrences, above the baseline of %d (%s) — see the individual lines this run also reported", constName, n, baseline, firstLines(found))}}
	case n < baseline:
		return []Violation{{Rule: rule, File: filepath.ToSlash(pkgDir),
			Message: fmt.Sprintf("%s: %d occurrences, below the baseline of %d: lower %s to %d", constName, n, baseline, constName, n)}}
	default:
		return nil
	}
}

func firstLines(vs []Violation) string {
	var parts []string
	for _, v := range vs {
		parts = append(parts, fmt.Sprintf("%s:%d", v.File, v.Line))
		if len(parts) == 5 {
			parts = append(parts, "…")
			break
		}
	}
	return strings.Join(parts, ", ")
}

// calleeName names a call's target regardless of whether it is a bare
// function or a method/selector call: newSendEngine(...) and
// s.buildWSStack(...) are both reached by Sel/Ident name alone, because this
// rule cares which function is calling, not through which receiver.
func calleeName(e ast.Expr) string {
	switch f := e.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// scanSiblingCtorPoints implements C1.
func scanSiblingCtorPoints(fset *token.FileSet, files []siblingSrcFile) []Violation {
	var out []Violation
	for _, sf := range files {
		for _, decl := range sf.f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch name := calleeName(call.Fun); name {
				case "newSendEngine", "newWSBroadcaster", "newSubscriberRegistry":
					// A second broadcaster or registry compiles and runs; its
					// frames just reach no client the Hub admitted.
					if fd.Name.Name != "buildWSStack" {
						out = append(out, Violation{Rule: "send_engine_sibling", File: filepath.ToSlash(sf.path),
							Line:    fset.Position(call.Pos()).Line,
							Message: fmt.Sprintf("%s called from %s, not buildWSStack: the engine, the broadcaster and its registry are siblings the composition root builds once (D7), not something another function may construct", name, fd.Name.Name)})
					}
				case "buildWSStack":
					if fd.Name.Name != "buildDashboard" {
						out = append(out, Violation{Rule: "send_engine_sibling", File: filepath.ToSlash(sf.path),
							Line:    fset.Position(call.Pos()).Line,
							Message: fmt.Sprintf("buildWSStack called from %s, not buildDashboard: it is the one composition-root step that assembles engine/broadcaster/Hub together", fd.Name.Name)})
					}
				}
				return true
			})
		}
	}
	return out
}

// engineReachAllowed reports whether a `.engine` selector's base expression
// is one of C2's three allowed shapes.
func engineReachAllowed(x ast.Expr, allowedIdents map[string]bool) bool {
	switch e := x.(type) {
	case *ast.Ident:
		return allowedIdents[e.Name]
	case *ast.SelectorExpr:
		id, ok := e.X.(*ast.Ident)
		return ok && id.Name == "hs" && e.Sel.Name == "wiring"
	}
	return false
}

// scanSiblingEngineReach implements C2.
func scanSiblingEngineReach(fset *token.FileSet, files []siblingSrcFile) []Violation {
	var out []Violation
	for _, sf := range files {
		for _, decl := range sf.f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			allowed := map[string]bool{}
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				rt := recvTypeName(fd.Recv.List[0].Type)
				if rt == "Hub" || rt == "SendHandler" {
					for _, n := range fd.Recv.List[0].Names {
						allowed[n.Name] = true
					}
				}
			}
			if fd.Type.Params != nil {
				for _, p := range fd.Type.Params.List {
					star, ok := p.Type.(*ast.StarExpr)
					if !ok {
						continue
					}
					id, ok := star.X.(*ast.Ident)
					if !ok || id.Name != "wiring" {
						continue
					}
					for _, n := range p.Names {
						if n.Name == "w" {
							allowed["w"] = true
						}
					}
				}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "engine" {
					return true
				}
				if engineReachAllowed(sel.X, allowed) {
					return true
				}
				out = append(out, Violation{Rule: "send_engine_sibling", File: filepath.ToSlash(sf.path),
					Line:    fset.Position(sel.Pos()).Line,
					Message: fmt.Sprintf("in %s: a `.engine` reach point whose base is not a *Hub/*SendHandler receiver, a `w *wiring` parameter, or `hs.wiring` — only the composition root and the two types the engine serves may reach it", fd.Name.Name)})
				return true
			})
		}
	}
	return out
}

// returnsStarSendEngine reports whether a function's result list includes
// *sendEngine.
func returnsStarSendEngine(ft *ast.FuncType) bool {
	if ft.Results == nil {
		return false
	}
	for _, r := range ft.Results.List {
		star, ok := r.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if id, ok := star.X.(*ast.Ident); ok && id.Name == "sendEngine" {
			return true
		}
	}
	return false
}

// scanSiblingAccessors implements C3: strict, no FuncDecl or FuncLit other
// than newSendEngine may return *sendEngine.
func scanSiblingAccessors(fset *token.FileSet, files []siblingSrcFile) []Violation {
	var out []Violation
	for _, sf := range files {
		ast.Inspect(sf.f, func(n ast.Node) bool {
			switch fn := n.(type) {
			case *ast.FuncDecl:
				if fn.Name.Name == "newSendEngine" {
					return true
				}
				if returnsStarSendEngine(fn.Type) {
					out = append(out, Violation{Rule: "send_engine_sibling", File: filepath.ToSlash(sf.path),
						Line:    fset.Position(fn.Pos()).Line,
						Message: fmt.Sprintf("%s returns *sendEngine: an accessor re-creates the Hub→engine→Hub cycle one call deep (D7 picked the sibling over this), only newSendEngine may hand one out", fn.Name.Name)})
				}
			case *ast.FuncLit:
				if returnsStarSendEngine(fn.Type) {
					out = append(out, Violation{Rule: "send_engine_sibling", File: filepath.ToSlash(sf.path),
						Line:    fset.Position(fn.Pos()).Line,
						Message: "a closure returns *sendEngine: an accessor re-creates the Hub→engine→Hub cycle one call deep, only newSendEngine may hand one out"})
				}
			}
			return true
		})
	}
	return out
}

// scanSiblingHolderWhitelist implements C4: strict, a *sendEngine field must
// be on the whitelist in sendEngineHolders.
func scanSiblingHolderWhitelist(fset *token.FileSet, files []siblingSrcFile) []Violation {
	var out []Violation
	for _, sf := range files {
		ast.Inspect(sf.f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				star, ok := fld.Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				id, ok := star.X.(*ast.Ident)
				if !ok || id.Name != "sendEngine" {
					continue
				}
				for _, fname := range fieldNames(fld) {
					if sendEngineHolders[ts.Name.Name][fname] {
						continue
					}
					out = append(out, Violation{Rule: "send_engine_sibling", File: filepath.ToSlash(sf.path),
						Line:    fset.Position(fld.Pos()).Line,
						Message: fmt.Sprintf("%s.%s holds a *sendEngine; only Hub.engine, SendHandler.engine, wiring.engine and HubOptions.Engine may (C4) — a new holder is a new way to pass the engine around outside the composition root", ts.Name.Name, fname)})
				}
			}
			return true
		})
	}
	return out
}

// fieldNames returns a field's declared names, or "" for an embedded field
// (an embedded *sendEngine would be a very strange thing to write, but the
// whitelist check must still name it in the violation).
func fieldNames(fld *ast.Field) []string {
	if len(fld.Names) == 0 {
		return []string{"<embedded>"}
	}
	var out []string
	for _, n := range fld.Names {
		out = append(out, n.Name)
	}
	return out
}

// scanSiblingHubNotifiers implements C5.
func scanSiblingHubNotifiers(fset *token.FileSet, files []siblingSrcFile) []Violation {
	var out []Violation
	for _, sf := range files {
		for _, decl := range sf.f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
				continue
			}
			if recvTypeName(fd.Recv.List[0].Type) != "Hub" || !hubNotifierMethods[fd.Name.Name] {
				continue
			}
			out = append(out, Violation{Rule: "send_engine_sibling", File: filepath.ToSlash(sf.path),
				Line:    fset.Position(fd.Pos()).Line,
				Message: fmt.Sprintf("*Hub declares %s: Hub must not be a sendNotifier (C5) — the broadcaster is the notifier, Hub only forwards to it", fd.Name.Name)})
		}
	}
	return out
}

// hasHubField reports whether st declares a field (named or embedded) of
// type Hub or *Hub.
func hasHubField(st *ast.StructType) (line token.Pos, found bool) {
	for _, fld := range st.Fields.List {
		switch t := fld.Type.(type) {
		case *ast.Ident:
			if t.Name == "Hub" {
				return fld.Pos(), true
			}
		case *ast.StarExpr:
			if id, ok := t.X.(*ast.Ident); ok && id.Name == "Hub" {
				return fld.Pos(), true
			}
		}
	}
	return 0, false
}

// scanSiblingNotifierBackpointer implements C6: strict, any type declaring
// broadcastState or broadcastSendError must not hold a Hub back-pointer.
func scanSiblingNotifierBackpointer(fset *token.FileSet, files []siblingSrcFile) []Violation {
	notifierTypes := map[string]bool{}
	for _, sf := range files {
		for _, decl := range sf.f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
				continue
			}
			if notifierBackpointerMethods[fd.Name.Name] {
				notifierTypes[recvTypeName(fd.Recv.List[0].Type)] = true
			}
		}
	}
	var out []Violation
	for _, sf := range files {
		ast.Inspect(sf.f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || !notifierTypes[ts.Name.Name] {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			if pos, found := hasHubField(st); found {
				out = append(out, Violation{Rule: "send_engine_sibling", File: filepath.ToSlash(sf.path),
					Line:    fset.Position(pos).Line,
					Message: fmt.Sprintf("%s declares broadcastState/broadcastSendError and holds a Hub field: the notifier would call back into the thing that calls it, rebuilding the cycle C5 removes one hop later (#2897 S5)", ts.Name.Name)})
			}
			return true
		})
	}
	return out
}

// scanSiblingFieldReads implements C7: inside a *Hub method, h.engine.X /
// h.bcast.X must be method calls, never field reads.
func scanSiblingFieldReads(fset *token.FileSet, files []siblingSrcFile) []Violation {
	var out []Violation
	for _, sf := range files {
		for _, decl := range sf.f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Recv == nil || len(fd.Recv.List) != 1 {
				continue
			}
			if recvTypeName(fd.Recv.List[0].Type) != "Hub" {
				continue
			}
			callees := map[*ast.SelectorExpr]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						callees[sel] = true
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || callees[sel] {
					return true
				}
				inner, ok := sel.X.(*ast.SelectorExpr)
				if !ok || (inner.Sel.Name != "engine" && inner.Sel.Name != "bcast") {
					return true
				}
				out = append(out, Violation{Rule: "send_engine_sibling", File: filepath.ToSlash(sf.path),
					Line:    fset.Position(sel.Pos()).Line,
					Message: fmt.Sprintf("%s reads %s field %q directly (C7): a *Hub method may call h.%s.X() but not read its fields", fd.Name.Name, inner.Sel.Name, sel.Sel.Name, inner.Sel.Name)})
				return true
			})
		}
	}
	return out
}

// scanSiblingBuildStepBcast implements C8: strict, a build*/New*/new*
// function may not read `<x>.hub.bcast`.
func scanSiblingBuildStepBcast(fset *token.FileSet, files []siblingSrcFile) []Violation {
	var out []Violation
	for _, sf := range files {
		for _, decl := range sf.f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || !isBuildStep(fd.Name.Name) {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "bcast" {
					return true
				}
				inner, ok := sel.X.(*ast.SelectorExpr)
				if !ok || inner.Sel.Name != "hub" {
					return true
				}
				out = append(out, Violation{Rule: "send_engine_sibling", File: filepath.ToSlash(sf.path),
					Line:    fset.Position(sel.Pos()).Line,
					Message: fmt.Sprintf("%s reads <x>.hub.bcast during construction (C8): construction-time code takes the broadcaster from wiring, not back off the Hub it is building", fd.Name.Name)})
				return true
			})
		}
	}
	return out
}
