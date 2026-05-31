package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// failingRunnablePlatform embeds the test mockPlatform and additionally
// implements RunnablePlatform.Start with a forced error so Start() takes the
// platform-start early-return path (server.go ~line 607) BEFORE the shutdown
// goroutine that owns close(shutdownComplete) is ever spawned.
type failingRunnablePlatform struct {
	*mockPlatform
	startErr error
}

func (f *failingRunnablePlatform) Start(_ platform.MessageHandler) error { return f.startErr }
func (f *failingRunnablePlatform) Stop() error                           { return nil }

// TestShutdownComplete_ClosesOnEarlyStartError pins R030056-GO-002: when
// Start() returns an error before spawning the shutdown goroutine (here via a
// RunnablePlatform whose Start fails), s.shutdownComplete must still be closed.
// The process-level shutdown sequencer (cmd/naozhi runShutdown) blocks
// unconditionally on `<-srv.ShutdownComplete()` in the server-error path; if
// the channel never closes, the whole process shutdown deadlocks. Before the
// fix the defer-close guard did not exist and this receive would block forever.
func TestShutdownComplete_ClosesOnEarlyStartError(t *testing.T) {
	t.Parallel()

	router := session.NewRouter(session.RouterConfig{})
	wantErr := errors.New("forced platform start failure")
	plat := &failingRunnablePlatform{mockPlatform: &mockPlatform{}, startErr: wantErr}

	srv := NewWithOptions(ServerOptions{
		Addr:      "127.0.0.1:0",
		Router:    router,
		Platforms: map[string]platform.Platform{"test": plat},
	})

	// Barrier must be open before Start.
	select {
	case <-srv.ShutdownComplete():
		t.Fatal("ShutdownComplete closed before Start ran")
	default:
	}

	err := srv.Start(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Start error = %v, want wrap of %v", err, wantErr)
	}

	// The defer-close guard must have closed the channel even though no
	// shutdown goroutine was ever spawned.
	select {
	case <-srv.ShutdownComplete():
		// closed as required
	case <-time.After(2 * time.Second):
		t.Fatal("ShutdownComplete did not close after Start early-returned an " +
			"error — runShutdown's unconditional receive would deadlock " +
			"(R030056-GO-002 regression)")
	}
}
