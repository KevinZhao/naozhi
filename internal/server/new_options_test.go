package server

import (
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
	if srv == nil {
		t.Fatal("NewWithOptions returned nil server for empty-Backend opts")
	}
	// R20260603-ARCH-1: the write-only backendTag field was removed; assert the
	// reply-tag resolution that feeds SessionHandlers.BackendTag directly.
	if tag := replyTagForBackend(""); tag != "cc" {
		t.Errorf("empty Backend should yield tag 'cc' (claude); got %q", tag)
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

// A nil Router panics at the constructor entry, naming the field (#3638).
func TestNewWithOptions_NilRouterPanicsAtEntry(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("buildServerWithHandlers(ServerOptions{}) did not panic")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "Router is required") {
			t.Fatalf("panic = %v, want it to name the missing Router", r)
		}
	}()
	buildServerWithHandlers(ServerOptions{Addr: ":0"})
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
	srv, hs := buildServerWithHandlers(ServerOptions{
		Addr:    ":0",
		Router:  router,
		Backend: "kiro",
		Identity: IdentityOptions{
			WorkspaceID:   "ws-a",
			WorkspaceName: "Alpha",
			Version:       "v0.0.1",
		},
	})
	// Backend selection must derive the kiro reply tag (R20260603-ARCH-1: the
	// write-only backendTag field was removed; assert the resolver directly).
	if tag := replyTagForBackend("kiro"); tag != "kiro" {
		t.Errorf("backendTag = %q, want kiro", tag)
	}
	// Same shape as backendTag above: Server.workspaceName was write-only and
	// removed in #2553 (nothing ever read s.workspaceName — the value goes
	// straight from opts into HealthHandler). Assert it reaches the consumer.
	if hs.healthH == nil || hs.healthH.workspaceName != "Alpha" {
		t.Errorf("healthH.workspaceName = %q, want Alpha", hs.healthH.workspaceName)
	}
	if srv.addr != ":0" {
		t.Errorf("addr = %q, want :0", srv.addr)
	}
	if srv.router == nil {
		t.Fatal("router must be set")
	}
}
