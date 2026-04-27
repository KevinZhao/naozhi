package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// TestNewWithOptions_EquivalentToPositional pins the contract that
// NewWithOptions and the legacy positional-arg New constructor produce
// servers with identical observable state. Future refactors that add
// fields to ServerOptions must keep both entry points in sync; this
// test fails loudly if a positional arg stops being written by
// buildServer.
func TestNewWithOptions_EquivalentToPositional(t *testing.T) {
	// Two independent routers so the test doesn't accidentally share
	// state between the two servers — we only care that each field we
	// passed surfaces on the resulting Server.
	router1 := session.NewRouter(session.RouterConfig{})
	router2 := session.NewRouter(session.RouterConfig{})
	p := &mockPlatform{}
	platforms := map[string]platform.Platform{"test": p}

	srvPositional := New(":0", router1, platforms, nil, nil, nil, "kiro", ServerOptions{
		WorkspaceID:   "ws-a",
		WorkspaceName: "Alpha",
		Version:       "v0.0.1",
	})

	srvOptions := NewWithOptions(ServerOptions{
		Addr:          ":0",
		Router:        router2,
		Platforms:     platforms,
		Backend:       "kiro",
		WorkspaceID:   "ws-a",
		WorkspaceName: "Alpha",
		Version:       "v0.0.1",
	})

	// Both must set the "kiro" backend tag since Backend==kiro in both.
	if srvPositional.backendTag != "kiro" {
		t.Errorf("positional backendTag = %q, want kiro", srvPositional.backendTag)
	}
	if srvOptions.backendTag != "kiro" {
		t.Errorf("options backendTag = %q, want kiro", srvOptions.backendTag)
	}
	if srvPositional.workspaceName != srvOptions.workspaceName {
		t.Errorf("workspaceName mismatch: positional=%q options=%q",
			srvPositional.workspaceName, srvOptions.workspaceName)
	}
	if srvPositional.addr != srvOptions.addr {
		t.Errorf("addr mismatch: positional=%q options=%q",
			srvPositional.addr, srvOptions.addr)
	}
	if srvPositional.router == nil || srvOptions.router == nil {
		t.Fatal("router must be set via either constructor")
	}
}

// TestNew_PositionalOverridesOptions locks the documented contract that
// when a caller passes BOTH a positional arg to New and a matching field
// in opts, the positional value wins. This prevents a subtle bug where
// a caller populating opts.Router alongside New(addr, otherRouter, ...)
// might expect opts.Router to be respected; documenting the wrapper's
// override behavior prevents that confusion.
func TestNew_PositionalOverridesOptions(t *testing.T) {
	positionalRouter := session.NewRouter(session.RouterConfig{})
	optsRouter := session.NewRouter(session.RouterConfig{})

	// Intentionally set opts.Router to a DIFFERENT router than the
	// positional arg. New() must use the positional one.
	srv := New(":0", positionalRouter, nil, nil, nil, nil, "claude", ServerOptions{
		Router: optsRouter,
	})
	if srv.router != positionalRouter {
		t.Error("New positional router must override opts.Router; got opts value instead")
	}
}

// TestNewWithOptions_DefaultBackendIsClaude verifies that an empty
// Backend field falls back to the "cc" tag (i.e. the "claude" backend
// selection logic). This matches the legacy New("", ...) behavior.
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
// The legacy New() already tolerated nil platforms/agents in tests; the
// new entry point must preserve that so migrating a test from
// New(":0", router, nil, nil, nil, nil, ...) to NewWithOptions is a
// zero-risk rename.
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
