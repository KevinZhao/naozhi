package server

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestServerLog_FallsBackToDefault pins the R247-ARCH-4 (#620) contract that a
// Server constructed without ServerOptions.Logger keeps logging through
// slog.Default(), so existing callers that rely on the SetDefault-in-main
// idiom are unaffected by the injection seam.
func TestServerLog_FallsBackToDefault(t *testing.T) {
	s := &Server{}
	if s.log() != slog.Default() {
		t.Fatalf("s.log() with no injected logger should return slog.Default()")
	}
}

// TestServerLog_UsesInjectedLogger pins that an injected logger is the one
// used — the writable seam that lets a future change hand each Server a
// component-scoped (and test-swappable) logger instead of the process global.
func TestServerLog_UsesInjectedLogger(t *testing.T) {
	var buf bytes.Buffer
	injected := slog.New(slog.NewTextHandler(&buf, nil))
	s := &Server{logger: injected}

	if s.log() != injected {
		t.Fatalf("s.log() should return the injected logger, not slog.Default()")
	}

	s.log().Info("scan tick", "count", 3)
	if got := buf.String(); !strings.Contains(got, "scan tick") || !strings.Contains(got, "count=3") {
		t.Fatalf("injected logger did not receive the record; got %q", got)
	}
}

// TestNewWithOptions_WiresLogger pins that ServerOptions.Logger reaches the
// constructed Server so the injection actually flows through buildServer.
func TestNewWithOptions_WiresLogger(t *testing.T) {
	injected := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	s := NewWithOptions(ServerOptions{
		Addr:    "127.0.0.1:0",
		Router:  session.NewRouter(session.RouterConfig{}),
		Backend: "claude",
		Logger:  injected,
	})
	if s.log() != injected {
		t.Fatalf("NewWithOptions did not thread ServerOptions.Logger into the Server")
	}
}
