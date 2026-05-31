package server

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// TestShutdownComplete_ClosesOnlyAfterDrain locks S11 (#390): the channel
// returned by ShutdownComplete() must (a) be non-nil and open immediately
// after construction — so a caller can read it before Start runs without a
// nil-channel block — and (b) close only after Start's shutdown goroutine
// has finished draining (i.e. after the shared ctx is cancelled). Before
// this barrier existed, cmd/naozhi raced router.Shutdown() against the HTTP
// drain; an in-flight GetOrCreate/Send handler could observe a half-cleaned
// session map.
func TestShutdownComplete_ClosesOnlyAfterDrain(t *testing.T) {
	t.Parallel()

	router := session.NewRouter(session.RouterConfig{})
	ready := make(chan struct{})
	srv := NewWithOptions(ServerOptions{
		Addr:    "127.0.0.1:0",
		Router:  router,
		OnReady: func() { close(ready) },
	})

	// Contract (a): channel exists and is open before Start.
	ch := srv.ShutdownComplete()
	if ch == nil {
		t.Fatal("ShutdownComplete() returned nil before Start — a caller reading it would block forever")
	}
	select {
	case <-ch:
		t.Fatal("ShutdownComplete closed before Start ever ran")
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan error, 1)
	go func() { startDone <- srv.Start(ctx) }()

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server never bound listener (OnReady not called) within 5s")
	}

	// While the server is up and ctx is live, the barrier must stay open.
	select {
	case <-srv.ShutdownComplete():
		t.Fatal("ShutdownComplete closed while server still running")
	default:
	}

	// Cancelling ctx triggers the drain; the barrier closes once it returns.
	cancel()
	select {
	case <-srv.ShutdownComplete():
	case <-time.After(10 * time.Second):
		t.Fatal("ShutdownComplete did not close within 10s after ctx cancel — drain barrier never fired")
	}

	// Start itself must also have returned.
	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after drain completed")
	}
}

// TestShutdownComplete_MainSequencesBeforeRouterShutdown is a source-level
// guardrail for S11 (#390): cmd/naozhi/main.go must block on
// srv.ShutdownComplete() BEFORE it calls router.Shutdown(). If a future edit
// reorders these (or drops the wait), router teardown again races the HTTP
// drain. Source-scan because the ordering is a happens-before contract the
// race detector cannot observe on its own.
func TestShutdownComplete_MainSequencesBeforeRouterShutdown(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/server/<this> → repo root → cmd/naozhi/main.go
	root := filepath.Clean(filepath.Join(filepath.Dir(self), "..", ".."))
	main := filepath.Join(root, "cmd", "naozhi", "main.go")
	raw, err := os.ReadFile(main)
	if err != nil {
		t.Fatalf("read %s: %v", main, err)
	}
	body := string(raw)

	// Anchor on the receive expression (the actual blocking wait) so a bare
	// mention of the method in a comment does not satisfy the contract.
	waitIdx := strings.Index(body, "<-srv.ShutdownComplete()")
	if waitIdx < 0 {
		t.Fatal("main.go: no `<-srv.ShutdownComplete()` wait found — S11 barrier dropped (#390)")
	}
	// Match the call as a statement (newline + indentation + call), not the
	// `scheduler.Stop()/router.Shutdown()` prose in the design comment above.
	routerIdx := strings.Index(body, "\n\t\t\trouter.Shutdown()")
	if routerIdx < 0 {
		t.Fatal("main.go: no router.Shutdown() statement found — shutdown contract changed")
	}
	if waitIdx >= routerIdx {
		t.Errorf("main.go: srv.ShutdownComplete() wait (offset %d) must appear BEFORE router.Shutdown() (offset %d) — S11 (#390) regressed: router teardown races the HTTP drain", waitIdx, routerIdx)
	}
}
