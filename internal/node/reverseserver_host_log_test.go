package node

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// RNEW-SEC-006 — /ws-node ServeHTTP must attach the incoming r.Host
// value to both the auth-failed slog.Warn and the registered slog.Info
// lines. Without it, a future operator investigating Host-based misuse
// (CDN misrouting, Host poisoning via a reverse proxy that leaks Host
// to upstream) has no forensic breadcrumb. The TODO's fallback
// "至少 slog.Warn 记录不预期 Host 值作回溯依据" is implemented here as
// an unconditional attr on both log lines — a per-Host allowlist
// requires config surface area out of scope for this change.

// TestReverseServer_Source_LogsHostOnAuthAndRegister is the primary
// regression guard: it parses reverseserver.go and asserts the two
// slog lines include a `host` attribute sourced from r.Host. A future
// refactor that drops the attr breaks this test at build time rather
// than silently eroding the forensic trail.
func TestReverseServer_Source_LogsHostOnAuthAndRegister(t *testing.T) {
	t.Parallel()
	_, thisFile, _, _ := runtime.Caller(0)
	src := filepath.Join(filepath.Dir(thisFile), "reverseserver.go")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read reverseserver.go: %v", err)
	}
	body := string(data)

	// Anchor 1: auth-failed branch — slog.Warn("reverse node auth failed"
	// ...) must include a `host` attr that reads from r.Host. The slog
	// call may span multiple lines and carry nested SanitizeForLog(...)
	// calls, so the pattern uses (?s) multiline + .*? between the message
	// literal and the two anchors we care about.
	authFail := regexp.MustCompile(
		`(?s)slog\.Warn\("reverse node auth failed".*?"host".*?r\.Host`)
	if !authFail.MatchString(body) {
		t.Error("auth-failed slog.Warn is missing \"host\" attr sourced from r.Host " +
			"— RNEW-SEC-006 regression; forensic breadcrumb for Host poisoning lost")
	}

	// Anchor 2: registered branch — slog.Info("reverse node registered"
	// ...) must include a `host` attr that reads from r.Host.
	reg := regexp.MustCompile(
		`(?s)slog\.Info\("reverse node registered".*?"host".*?r\.Host`)
	if !reg.MatchString(body) {
		t.Error("registered slog.Info is missing \"host\" attr sourced from r.Host " +
			"— RNEW-SEC-006 regression; forensic breadcrumb for Host poisoning lost")
	}
}

// TestReverseServer_AuthFailed_LogsHost is a behavioural regression: it
// captures slog output during a wrong-token auth failure and asserts
// the line contains host=<test server authority>. Together with the
// source-level anchor this pins both the source-position and the
// observable log content.
func TestReverseServer_AuthFailed_LogsHost(t *testing.T) {
	buf, mu, restore := captureSlog(t)
	defer restore()

	rs := newTestReverseServer("node-1", "secret", false)
	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn := dialReverseNode(t, srv)
	defer conn.Close()

	// Wrong token → triggers the "reverse node auth failed" log path.
	resp := reverseAuth(t, conn, "node-1", "WRONG-TOKEN", "")
	if resp.Type == "registered" {
		t.Fatalf("expected auth failure, got registered")
	}

	waitForLog(t, buf, mu, "reverse node auth failed")

	mu.Lock()
	logged := buf.String()
	mu.Unlock()

	if !strings.Contains(logged, "host=") {
		t.Errorf("auth-failed log missing host= attr; got:\n%s", logged)
	}
}

// TestReverseServer_Registered_LogsHost pairs with the auth-failed
// behavioural test: the happy path's "reverse node registered" log
// must also carry the host attr.
func TestReverseServer_Registered_LogsHost(t *testing.T) {
	buf, mu, restore := captureSlog(t)
	defer restore()

	rs := newTestReverseServer("node-1", "secret", false)
	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn := dialReverseNode(t, srv)
	defer conn.Close()

	resp := reverseAuth(t, conn, "node-1", "secret", "worker.internal")
	if resp.Type != "registered" {
		t.Fatalf("expected registered, got %q (err %q)", resp.Type, resp.Error)
	}

	waitForLog(t, buf, mu, "reverse node registered")

	mu.Lock()
	logged := buf.String()
	mu.Unlock()

	if !strings.Contains(logged, "host=") {
		t.Errorf("registered log missing host= attr; got:\n%s", logged)
	}
}

// captureSlog swaps slog.Default() for a handler writing into a
// test-owned bytes.Buffer. Returns the buffer, its guarding mutex, and
// a restore closure the caller defers to reinstate the prior default.
func captureSlog(t *testing.T) (*bytes.Buffer, *sync.Mutex, func()) {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	handler := slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	return &buf, &mu, func() { slog.SetDefault(prev) }
}

// lockedWriter serializes writes to a bytes.Buffer so the test can
// safely read buf.String() from the test goroutine while slog is
// writing from the reverseserver handler goroutine.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}

// waitForLog polls the capture buffer until the expected substring
// appears or a bounded budget elapses. Prevents behavioural tests from
// flaking on slow schedulers where the post-register log lands after
// the main goroutine returns.
func waitForLog(t *testing.T, buf *bytes.Buffer, mu *sync.Mutex, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		has := strings.Contains(buf.String(), want)
		mu.Unlock()
		if has {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for log %q to appear", want)
}
