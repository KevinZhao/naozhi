package cron

// TestTranscriptIOLogUsesBasename is a static-analysis contract test for
// R20260602141221-SEC-13: the two non-security slog.Warn calls in
// transcript.go that report IO errors (line_too_long / scan_io_error) must
// log filepath.Base(resolved) rather than the full `resolved` path, which
// would leak operator home/workspace/session-UUID to log aggregators.
//
// The escape-attempt log (security event) intentionally retains the full path
// and is NOT covered by this test.
//
// Strategy: parse transcript.go with go/ast and look for slog.Warn call-sites
// whose message contains "line too long" or "scan io error". For each such
// site, verify that no argument is the bare identifier `resolved` — every
// path attribute must be wrapped in filepath.Base(…).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTranscriptIOLogUsesBasename(t *testing.T) {
	t.Parallel()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src := filepath.Join(filepath.Dir(thisFile), "transcript.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse transcript.go: %v", err)
	}

	// ioErrorMessages are the log messages whose call-sites must not expose
	// a bare `resolved` identifier.
	ioErrorMessages := map[string]bool{
		"cron transcript: line too long (returning partial)": true,
		"cron transcript: scan io error (returning partial)": true,
	}

	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// Only slog.Warn calls.
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "slog" || sel.Sel.Name != "Warn" {
			return true
		}
		// First argument must be the message string.
		if len(call.Args) == 0 {
			return true
		}
		msgLit, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		// Strip surrounding quotes.
		msg := msgLit.Value
		if len(msg) >= 2 {
			msg = msg[1 : len(msg)-1]
		}
		if !ioErrorMessages[msg] {
			return true
		}

		// This is one of the IO-error slog sites. Walk all remaining args
		// looking for a bare `resolved` identifier — that would mean the
		// full path leaked.
		for _, arg := range call.Args[1:] {
			if ident, ok := arg.(*ast.Ident); ok && ident.Name == "resolved" {
				pos := fset.Position(ident.Pos())
				t.Errorf("%s: slog.Warn(%q) passes bare `resolved` (full path); wrap with filepath.Base(resolved) [R20260602141221-SEC-13]", pos, msg)
			}
		}
		return true
	})
}
