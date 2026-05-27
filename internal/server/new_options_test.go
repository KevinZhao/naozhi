package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestNewWithOptions_DefaultBackendIsClaude verifies that an empty
// Backend field falls back to the "cc" tag (i.e. the "claude" backend
// selection logic). Pre-R237-ARCH-14 this test had a sibling that
// pinned the legacy New(":0", ...) shape; that wrapper has been deleted
// (#614) and only the options entry point remains.
func TestNewWithOptions_DefaultBackendIsClaude(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{
		Addr:   ":0",
		Router: router,
		// Backend deliberately empty
	})
	if srv.backendTag != "cc" {
		t.Errorf("empty Backend should yield tag 'cc' (claude); got %q", srv.backendTag)
	}
}

// TestNewWithOptions_NilMapsTolerated ensures the constructor does not
// panic when Platforms / Agents / AgentCommands / Nodes are all nil.
// Maps build a Server cleanly with only a router + addr — the legitimate
// "smoke-test fixture" shape relied on by every server unit test.
func TestNewWithOptions_NilMapsTolerated(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{
		Addr:   ":0",
		Router: router,
	})
	if srv == nil {
		t.Fatal("NewWithOptions returned nil server for minimal opts")
	}
}

// TestNewWithOptions_FieldsRoundTrip pins the contract that ServerOptions
// fields surface verbatim on the resulting *Server. Replaces the prior
// dual-constructor equivalence test now that the legacy positional-arg
// `New` has been deleted (R237-ARCH-14 / #614 removal). Without this we
// would lose coverage for "did buildServer remember to read opts.X" —
// which is the actual invariant that the dual-test was indirectly
// guarding.
func TestNewWithOptions_FieldsRoundTrip(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{
		Addr:          ":0",
		Router:        router,
		Backend:       "kiro",
		WorkspaceID:   "ws-a",
		WorkspaceName: "Alpha",
		Version:       "v0.0.1",
	})
	// Backend selection must derive the kiro reply tag.
	if srv.backendTag != "kiro" {
		t.Errorf("backendTag = %q, want kiro", srv.backendTag)
	}
	if srv.workspaceName != "Alpha" {
		t.Errorf("workspaceName = %q, want Alpha", srv.workspaceName)
	}
	if srv.addr != ":0" {
		t.Errorf("addr = %q, want :0", srv.addr)
	}
	if srv.router == nil {
		t.Fatal("router must be set")
	}
}

// TestServerNew_NotReintroduced is the regression guard that replaces
// the prior TestLegacyServerNew_NoNewCallSites + TestServerNew_Marked
// Deprecated pair. Both pre-deletion tests existed to keep the
// `func New(addr, ...)` shim from spreading; with the shim removed the
// failure mode shifts from "callers grow" to "someone re-adds the
// shim". This test fails the build if either:
//
//  1. server.go gains a function whose signature begins with
//     `func New(addr string` (the legacy shape we just removed).
//  2. A non-test *.go file in this package adds a `New(":0",`
//     call site (would re-introduce a positional dependency).
//
// R237-ARCH-14 / #614.
func TestServerNew_NotReintroduced(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	// (1) The legacy func definition shape must NOT come back. Scan
	// only at start-of-line so the deletion-rationale godoc above
	// `buildServer` (which references `func New(addr string, ...)` in
	// prose) does not generate a false positive — Go function defs
	// always start at column 0, so a `^func New(addr string` anchor
	// distinguishes definitions from references.
	const defNeedle = "\nfunc New(addr string"
	if strings.Contains("\n"+string(data), defNeedle) {
		t.Error("server.go must not redefine `func New(addr string ...)` — use NewWithOptions(ServerOptions{...}) per R237-ARCH-14 (#614)")
	}

	// (2) Scan every *.go file in the server package directory for the
	// canonical positional-args call shape `New(":0",`. The previous
	// allow-list (new_options_test.go) is no longer needed because the
	// equivalence test that depended on the wrapper has been rewritten
	// to live entirely in terms of NewWithOptions.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	const needle = `New(":0",`
	var offenders []string
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || filepath.Ext(name) != ".go" {
			continue
		}
		// Skip the regression-guard file itself — its godoc references
		// the literal in commentary which would otherwise be a false
		// positive. The scan still covers EVERY other file so an
		// accidental call-site addition does not slip through.
		if name == "new_options_test.go" {
			continue
		}
		fileData, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(fileData), needle) {
			offenders = append(offenders, name)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("legacy `New(\":0\", ...)` call sites present in: %v — the legacy positional-args wrapper was deleted in R237-ARCH-14 (#614); use NewWithOptions(ServerOptions{...})", offenders)
	}
}
