package server

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// TestAllowedRootEmpty_TokenPublic_LogsAtError pins R237-SEC-9 / #658:
// when allowed_root is unset AND a dashboard_token is configured AND the
// bind address is non-loopback, the second log line MUST emit at
// slog.LevelError rather than Warn. Operators wire alert pipelines /
// journald PRIORITY filters to Error; downgrading this back to Warn
// would silently re-hide a high-severity multi-user misconfiguration.
//
// We exercise the real NewWithOptions path so the test catches a
// regression where the slog call is removed/downgraded — replaying the
// message in the test would only catch text drift, not the actual
// branch wiring.
//
// Not t.Parallel: this test swaps slog.Default to capture log output.
// Running concurrently with other tests that emit slog records races on
// the captured buffer.
func TestAllowedRootEmpty_TokenPublic_LogsAtError(t *testing.T) {
	// Pin the precondition: isPlaintextPublicAddr must classify the test
	// address as public, otherwise the Error branch never fires and the
	// test would silently pass for the wrong reason.
	const publicAddr = "0.0.0.0:8180"
	if !isPlaintextPublicAddr(publicAddr) {
		t.Fatalf("test prerequisite broken: isPlaintextPublicAddr(%q) must be true", publicAddr)
	}

	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)
	// Wrap the JSONHandler buffer in a mutex so concurrent slog writes from
	// any goroutine NewWithOptions spawns during init don't tear records.
	handler := slog.NewJSONHandler(&lockedWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)

	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": &mockPlatform{}}

	// Trigger the exact branch:
	//   AllowedRoot=="" && DashboardToken!="" && isPlaintextPublicAddr(Addr)
	// Use a no-loopback addr + a token so isPlaintextPublicAddr returns true
	// and the second slog call (the upgraded ERROR) fires.
	srv := NewWithOptions(ServerOptions{
		Addr:           publicAddr,
		Router:         router,
		Platforms:      platforms,
		DashboardToken: "tok-required-to-trigger-error-branch",
		AllowedRoot:    "", // explicit: unset
		Backend:        "claude",
	})
	if srv == nil {
		t.Fatal("NewWithOptions returned nil")
	}

	mu.Lock()
	out := buf.String()
	mu.Unlock()

	// Assert the first line (Warn) is present so we know the harness
	// captured the AllowedRoot=="" block at all.
	if !strings.Contains(out, `"level":"WARN"`) {
		t.Fatalf("expected WARN record from allowed_root=\"\" branch; capture=%s", out)
	}
	if !strings.Contains(out, "server.allowed_root is unset") {
		t.Fatalf("expected first allowed_root warning text in capture; got: %s", out)
	}
	// The high-severity branch MUST be ERROR. We anchor on the unique
	// substring of the upgraded message so unrelated ERROR records that
	// might fire during NewWithOptions init can't false-positive us.
	if !strings.Contains(out, `"level":"ERROR"`) {
		t.Fatalf("R237-SEC-9 regression: token+public allowed_root warning must be slog.Error; capture lacked any ERROR record:\n%s", out)
	}
	if !strings.Contains(out, "token-protected, network-reachable dashboard") {
		t.Fatalf("R237-SEC-9 regression: high-severity message body changed — operator alerting pipelines key on this text; full output:\n%s", out)
	}
}

// lockedWriter serialises writes to the underlying buffer so slog records
// emitted from concurrent goroutines during NewWithOptions init don't
// interleave or race the test's read.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
