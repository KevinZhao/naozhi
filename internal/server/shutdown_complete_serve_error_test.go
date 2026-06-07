package server

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// errAcceptListener wraps a real bound net.Listener but fails Accept with a
// fatal (non-temporary) error, so http.Server.Serve returns that error rather
// than ErrServerClosed — driving Start down the R20260531-GO-001 branch.
type errAcceptListener struct {
	net.Listener
	err error
}

func (l *errAcceptListener) Accept() (net.Conn, error) { return nil, l.err }

// TestShutdownComplete_ClosesOnServeError pins R20260531-GO-001: when
// srv.Serve returns a non-ErrServerClosed error, Start must cancel its
// internal serveCtx so the shutdown goroutine (sole closer of
// shutdownComplete) wakes, drains, and closes the channel — even though the
// caller's ctx is never cancelled. Before the fix the goroutine blocked on
// ctx.Done() forever and any reader of ShutdownComplete() deadlocked.
func TestShutdownComplete_ClosesOnServeError(t *testing.T) {
	fatalAccept := errors.New("forced fatal accept failure")

	// Pin $HOME (and CLAUDE_PROJECTS_DIR) to an empty temp dir BEFORE
	// NewWithOptions resolves ~/.claude. Otherwise NewWithOptions kicks off
	// WarmHistoryCache against the host's real ~/.claude, and on a machine
	// with a large session history the background scan can take >5s under
	// -race. The shutdown goroutine blocks on WaitWarmHistory() during drain,
	// so a slow scan stalls Start's return past the 5s assertion below and
	// keeps the Start goroutine alive — which then races the t.Cleanup that
	// restores the package-global listenTCP (R20260531-GO-001 / #1934). An
	// empty home makes the scan return immediately, isolating the test from
	// host state and from that cleanup race.
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("CLAUDE_PROJECTS_DIR", filepath.Join(emptyHome, ".claude", "projects"))

	// Inject a listener that binds fine but fails Accept fatally.
	prev := listenTCP
	listenTCP = func(network, addr string) (net.Listener, error) {
		real, err := prev(network, addr)
		if err != nil {
			return nil, err
		}
		return &errAcceptListener{Listener: real, err: fatalAccept}, nil
	}
	t.Cleanup(func() { listenTCP = prev })

	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{Addr: "127.0.0.1:0", Router: router})

	// Caller ctx is deliberately NOT cancelled — the deadlock the fix
	// prevents is exactly the "Serve errored but ctx still live" case.
	ctx := context.Background()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	select {
	case err := <-errCh:
		if !errors.Is(err, fatalAccept) {
			t.Fatalf("Start error = %v, want wrap of %v", err, fatalAccept)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Serve errored — serveCtx was never " +
			"cancelled (R20260531-GO-001 regression)")
	}

	// shutdownComplete must be closed: Start's error branch waits on it
	// before returning, so a successful Start return already implies this,
	// but assert directly for clarity / future-proofing.
	select {
	case <-srv.ShutdownComplete():
	case <-time.After(2 * time.Second):
		t.Fatal("ShutdownComplete did not close after Serve error")
	}
}
