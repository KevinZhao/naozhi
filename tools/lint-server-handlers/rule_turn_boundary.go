// Rule turn_boundary (#2897 T3004 A1): gates the turn-orchestration merge
// land BEFORE any of the structural moves they protect, per #3004's design.
// Four checks, all AST-only over non-test production files, all ratchets
// (own *Baseline constant, both directions checked — a lowered count without
// a lowered constant fails exactly like a raised one):
//
//	G-a ctx marker: call sites of WithPassthrough/IsPassthrough/WithUrgent/
//	   IsUrgent (never their own func declarations — an *ast.CallExpr-only
//	   walk cannot match a FuncDecl), plus context.WithValue( call sites.
//	   Scans dispatch/server/turn, because turn is where C1's Orchestrator
//	   legitimately reads request-level fields instead of a ctx marker — the
//	   count must still see it appear there if someone backslides.
//	G-b queue escape: .Enqueue(/.DoneOrDrain( call sites in dispatch/server
//	   ONLY. Deliberately excludes turn: once B moves MessageQueue there,
//	   turn's own Submit/loop implementation calls these legitimately, and
//	   G-b is asking whether anything OUTSIDE turn still reaches around the
//	   port.
//	G-d slash literals: the six `"/new"`/`"/new "`/`"/clear"`/`"/clear "`/
//	   `"/urgent"`/`"/urgent "` string literals in dispatch/server ONLY.
//	   Deliberately excludes turn: turn/parse.go is their one sanctioned home
//	   from C1 onward (#3004 分叉 3/6), so a literal appearing there is not
//	   a boundary violation and must not block that PR's own gate value.
//
// G-c (the matching reflect check that *MessageQueue's and
// dispatch.SessionRouter's exported method sets equal an explicit list of
// names) lives in internal/dispatch/queue_surface_test.go instead: it reads
// no source file, so it runs under `go test`, not this AST scan.
//
// internal/turn does not exist until #2897 T3004-C1. turnBoundaryDirs mirrors
// the dirExists filter lateSetterPkgs already uses for session/cron/sysession
// upstream: an absent turn directory is scanned as empty, which is correct
// today (every one of G-a/G-b/G-d's baseline occurrences already sits in
// dispatch/server, matching #3004's measured state) and stays correct once
// C1 adds the directory, because the three checks above read it (or
// deliberately don't) by design, not by its mere presence. The dispatch and
// server directories are not optional: scanTurnBoundary reports a Violation
// (rather than silently scanning nothing) if either is unreadable, so a
// misconfigured -server-pkg cannot narrow G-a/G-b/G-d's scope the way it
// would be allowed to for the optional dashboard package.
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

// turnCtxMarkerBaseline is G-a's first slice: call sites of the four ctx
// marker functions. 6 on master (dispatch.go:526, commands.go:221 ×2,
// send.go:58/92/355) — #3004's measured state. Falls to 3 at C2 (dispatch's
// three calls move into turn), 0 at D (send.go's three calls go with it;
// WithPassthrough/IsPassthrough/WithUrgent/IsUrgent are deleted).
const turnCtxMarkerBaseline = 6

// turnCtxWithValueBaseline is G-a's second slice: internal/dispatch's and
// internal/server's own context.WithValue( call sites (not stdlib's, not
// turn's — turn's Admission port may reasonably need one of its own). 1
// today: withSendOpts in passthrough_ctx.go. Falls to 0 when D deletes that
// file.
const turnCtxWithValueBaseline = 1

// turnQueueEscapeBaseline is G-b: .Enqueue(/.DoneOrDrain( call sites in
// dispatch/server. 4 today (dispatch.go:538/631, send.go:255,
// send_owner_loop.go:44). B does not change this count (the callers don't
// move, only the type's package does); C2 drops it to 2 (IM's two calls move
// into turn.Orchestrator.Submit/the drain loop); D drops it to 0.
const turnQueueEscapeBaseline = 4

// turnSlashLiteralBaseline is G-d: occurrences of the six slash-command
// string literals in dispatch/server. 11 today (commands.go:108/109/117/
// 118/122, send.go:145/239/240 — #3004's measured state, confirmed on this
// branch). Falls as turn.Parse absorbs each side's parsing (C2, then D),
// reaching 0 once only turn/parse.go holds them.
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

// scanTurnBoundary implements rule turn_boundary: G-a, G-b and G-d.
// dispatchPkg/turnPkg are derived from serverPkg's parent directory, mirroring
// lateSetterPkgs. dispatch is required (a read error is reported, not
// swallowed); turn is optional until C1 and skipped if absent.
func scanTurnBoundary(serverPkg string) []Violation {
	parent := filepath.Dir(serverPkg)
	dispatchPkg := filepath.Join(parent, "dispatch")
	turnPkg := filepath.Join(parent, "turn")

	fset := token.NewFileSet()
	dispatchFiles, err := parseProductionGoFiles(fset, dispatchPkg)
	if err != nil {
		return []Violation{{Rule: "turn_boundary", File: filepath.ToSlash(dispatchPkg), Message: err.Error()}}
	}
	serverFiles, err := parseProductionGoFiles(fset, serverPkg)
	if err != nil {
		return []Violation{{Rule: "turn_boundary", File: filepath.ToSlash(serverPkg), Message: err.Error()}}
	}
	var turnFiles []turnBoundarySrcFile
	if dirExists(turnPkg) {
		turnFiles, err = parseProductionGoFiles(fset, turnPkg)
		if err != nil {
			return []Violation{{Rule: "turn_boundary", File: filepath.ToSlash(turnPkg), Message: err.Error()}}
		}
	}

	// G-a scans dispatch + server + turn.
	gaFiles := append(append([]turnBoundarySrcFile{}, dispatchFiles...), serverFiles...)
	gaFiles = append(gaFiles, turnFiles...)
	// G-b and G-d scan dispatch + server only (see package doc for why turn
	// is excluded from both).
	gbdFiles := append(append([]turnBoundarySrcFile{}, dispatchFiles...), serverFiles...)

	var out []Violation
	out = append(out, ratchetViolation("turn_boundary", "turnCtxMarkerBaseline", turnCtxMarkerBaseline, scanCtxMarkerCalls(fset, gaFiles), serverPkg)...)
	out = append(out, ratchetViolation("turn_boundary", "turnCtxWithValueBaseline", turnCtxWithValueBaseline, scanContextWithValue(fset, gaFiles), serverPkg)...)
	out = append(out, ratchetViolation("turn_boundary", "turnQueueEscapeBaseline", turnQueueEscapeBaseline, scanQueueEscapeCalls(fset, gbdFiles), serverPkg)...)
	out = append(out, ratchetViolation("turn_boundary", "turnSlashLiteralBaseline", turnSlashLiteralBaseline, scanSlashLiterals(fset, gbdFiles), serverPkg)...)
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
				Message: "context.WithValue in dispatch/server: the ctx marker it backs is meant to go, not grow a second value key (#3004 G-a)"})
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
				Message: fmt.Sprintf("%s called outside internal/turn: the queue's drain protocol is meant to be reached through the turn port, not directly by dispatch/server (#3004 G-b)", sel.Sel.Name)})
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
