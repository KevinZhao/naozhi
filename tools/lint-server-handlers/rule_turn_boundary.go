// Rule turn_boundary gates #3004's turn-orchestration merge before any of
// its structural moves land (#2897 T3004 A1). Three checks, AST-only over
// non-test production files, each a ratchet with its own *Baseline constant
// checked in both directions (a lowered count without a lowered constant
// fails exactly like a raised one):
//
//	G-a ctx marker: call sites of WithPassthrough/IsPassthrough/WithUrgent/
//	   IsUrgent, bare or qualified (never their own func declarations — an
//	   *ast.CallExpr-only walk cannot match a FuncDecl), in dispatch, server,
//	   turn and upstream; plus context.WithValue( call sites in dispatch,
//	   server and turn.
//	G-b queue escape: .Enqueue(/.DoneOrDrain( call sites in dispatch, server
//	   and upstream. turn is excluded: it is where the queue's own drain
//	   protocol is meant to live, so G-b asks whether anything outside turn
//	   still reaches around the port.
//	G-d slash literals: the six "/new"/"/clear"/"/urgent" (with and without
//	   a trailing space) string literals in dispatch/server. turn is
//	   excluded: turn/parse.go is their one sanctioned home.
//
// upstream is scanned by G-a's marker slice and G-b because its send RPC is
// the third turn entry (#3004 decision 6, deferred to its own issue): the
// most likely place for a new caller to bypass the port unseen.
//
// G-c (turn.Queue's and dispatch.SessionRouter's exported method sets) lives
// in the queue_surface_test.go files of internal/turn and internal/dispatch:
// it reads no source, so it runs under `go test`.
//
// dispatch, server and turn are required: an unreadable one is reported, so
// a misconfigured -server-pkg cannot narrow the scope. upstream is scanned
// only if present.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// turnCtxMarkerBaseline is G-a's first slice: ctx marker call sites. 6 is
// #3004's measured state; it only goes down (#3004 has the per-phase plan).
const turnCtxMarkerBaseline = 6

// turnCtxWithValueBaseline is G-a's second slice: context.WithValue( call
// sites in dispatch, server and turn (today only withSendOpts in
// dispatch/passthrough_ctx.go). 1 is #3004's measured state; the target is 0.
const turnCtxWithValueBaseline = 1

// turnQueueEscapeBaseline is G-b: .Enqueue(/.DoneOrDrain( call sites outside
// turn. 4 is #3004's measured state; the target is 0.
const turnQueueEscapeBaseline = 4

// turnSlashLiteralBaseline is G-d: slash-command literal occurrences in
// dispatch/server. 11 is #3004's measured state; the target is 0.
const turnSlashLiteralBaseline = 11

// ctxMarkerCallNames are G-a's first slice: the four ctx marker functions
// (#3004 分叉 8). A bare Ident covers an unqualified call from inside
// dispatch itself; calleeName (rule_send_engine_sibling.go) also matches the
// qualified form server's send.go uses (dispatch.IsUrgent(ctx)).
var ctxMarkerCallNames = map[string]bool{
	"WithPassthrough": true,
	"IsPassthrough":   true,
	"WithUrgent":      true,
	"IsUrgent":        true,
}

// turnQueueEscapeMethods are G-b's two method names.
var turnQueueEscapeMethods = map[string]bool{
	"Enqueue":     true,
	"DoneOrDrain": true,
}

// turnSlashLiterals are G-d's six literal forms (#3004 现状: commands.go's
// normalizeSlashCommand and send.go's inline checks each spell these out
// separately; turn.Parse is meant to be the one place that still does).
var turnSlashLiterals = map[string]bool{
	"/new":     true,
	"/new ":    true,
	"/clear":   true,
	"/clear ":  true,
	"/urgent":  true,
	"/urgent ": true,
}

type turnBoundarySrcFile struct {
	path string
	f    *ast.File
}

// parseProductionGoFiles parses every non-test .go file directly inside dir
// (one level, matching this module's flat per-package directories — the same
// assumption parseSiblingPkgDir makes for rule_send_engine_sibling.go).
func parseProductionGoFiles(fset *token.FileSet, dir string) ([]turnBoundarySrcFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []turnBoundarySrcFile
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		path := filepath.Join(dir, n)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		files = append(files, turnBoundarySrcFile{path, f})
	}
	return files, nil
}

// scanTurnBoundary implements rule turn_boundary: G-a, G-b and G-d. The
// sibling package directories are derived from serverPkg's parent, mirroring
// lateSetterPkgs.
func scanTurnBoundary(serverPkg string) []Violation {
	parent := filepath.Dir(serverPkg)
	fset := token.NewFileSet()
	parseDir := func(dir string, required bool) ([]turnBoundarySrcFile, []Violation) {
		if !required && !dirExists(dir) {
			return nil, nil
		}
		files, err := parseProductionGoFiles(fset, dir)
		if err != nil {
			return nil, []Violation{{Rule: "turn_boundary", File: filepath.ToSlash(dir), Message: err.Error()}}
		}
		return files, nil
	}
	dispatchFiles, errV := parseDir(filepath.Join(parent, "dispatch"), true)
	if errV != nil {
		return errV
	}
	serverFiles, errV := parseDir(serverPkg, true)
	if errV != nil {
		return errV
	}
	turnFiles, errV := parseDir(filepath.Join(parent, "turn"), true)
	if errV != nil {
		return errV
	}
	upstreamFiles, errV := parseDir(filepath.Join(parent, "upstream"), false)
	if errV != nil {
		return errV
	}

	join := func(sets ...[]turnBoundarySrcFile) []turnBoundarySrcFile {
		var out []turnBoundarySrcFile
		for _, s := range sets {
			out = append(out, s...)
		}
		return out
	}
	markerFiles := join(dispatchFiles, serverFiles, turnFiles, upstreamFiles)
	withValueFiles := join(dispatchFiles, serverFiles, turnFiles)
	queueFiles := join(dispatchFiles, serverFiles, upstreamFiles)
	slashFiles := join(dispatchFiles, serverFiles)

	var out []Violation
	out = append(out, ratchetViolation("turn_boundary", "turnCtxMarkerBaseline", turnCtxMarkerBaseline, scanCtxMarkerCalls(fset, markerFiles), serverPkg)...)
	out = append(out, ratchetViolation("turn_boundary", "turnCtxWithValueBaseline", turnCtxWithValueBaseline, scanContextWithValue(fset, withValueFiles), serverPkg)...)
	out = append(out, ratchetViolation("turn_boundary", "turnQueueEscapeBaseline", turnQueueEscapeBaseline, scanQueueEscapeCalls(fset, queueFiles), serverPkg)...)
	out = append(out, ratchetViolation("turn_boundary", "turnSlashLiteralBaseline", turnSlashLiteralBaseline, scanSlashLiterals(fset, slashFiles), serverPkg)...)
	return out
}

// scanCtxMarkerCalls implements G-a's first slice.
func scanCtxMarkerCalls(fset *token.FileSet, files []turnBoundarySrcFile) []Violation {
	var out []Violation
	for _, sf := range files {
		ast.Inspect(sf.f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name := calleeName(call.Fun); ctxMarkerCallNames[name] {
				out = append(out, Violation{Rule: "turn_boundary", File: filepath.ToSlash(sf.path),
					Line:    fset.Position(call.Pos()).Line,
					Message: fmt.Sprintf("call to %s: control flow travels through a ctx marker instead of an explicit field (#3004 分叉 8) — turn.SendSpec is meant to replace it", name)})
			}
			return true
		})
	}
	return out
}

// scanContextWithValue implements G-a's second slice.
func scanContextWithValue(fset *token.FileSet, files []turnBoundarySrcFile) []Violation {
	var out []Violation
	for _, sf := range files {
		ast.Inspect(sf.f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "WithValue" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "context" {
				return true
			}
			out = append(out, Violation{Rule: "turn_boundary", File: filepath.ToSlash(sf.path),
				Line:    fset.Position(call.Pos()).Line,
				Message: "context.WithValue in dispatch/server/turn: the ctx marker it backs is meant to go, not grow a second value key (#3004 G-a)"})
			return true
		})
	}
	return out
}

// scanQueueEscapeCalls implements G-b.
func scanQueueEscapeCalls(fset *token.FileSet, files []turnBoundarySrcFile) []Violation {
	var out []Violation
	for _, sf := range files {
		ast.Inspect(sf.f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !turnQueueEscapeMethods[sel.Sel.Name] {
				return true
			}
			out = append(out, Violation{Rule: "turn_boundary", File: filepath.ToSlash(sf.path),
				Line:    fset.Position(call.Pos()).Line,
				Message: fmt.Sprintf("%s called outside internal/turn: the queue's drain protocol is meant to be reached through the turn port, not directly (#3004 G-b)", sel.Sel.Name)})
			return true
		})
	}
	return out
}

// scanSlashLiterals implements G-d.
func scanSlashLiterals(fset *token.FileSet, files []turnBoundarySrcFile) []Violation {
	var out []Violation
	for _, sf := range files {
		ast.Inspect(sf.f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil || !turnSlashLiterals[v] {
				return true
			}
			out = append(out, Violation{Rule: "turn_boundary", File: filepath.ToSlash(sf.path),
				Line:    fset.Position(lit.Pos()).Line,
				Message: fmt.Sprintf("slash-command literal %q in dispatch/server: turn/parse.go is the one sanctioned home for these from C1 onward (#3004 分叉 3/6)", v)})
			return true
		})
	}
	return out
}
