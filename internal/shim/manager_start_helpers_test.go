package shim

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// nopReadCloser wraps a bytes.Buffer so we can hand it to waitForShimReady
// as an io.ReadCloser without spinning up a real exec.Cmd pipe.
type nopReadCloser struct {
	io.Reader
	closed bool
}

func (n *nopReadCloser) Close() error { n.closed = true; return nil }

// blockingReadCloser blocks Read indefinitely until Close is called, mirroring
// what a still-running shim's stdout pipe looks like before it emits the
// ready frame. Used to exercise the ctx-cancel and timeout branches.
type blockingReadCloser struct {
	closed chan struct{}
	once   bool
}

func newBlockingRC() *blockingReadCloser {
	return &blockingReadCloser{closed: make(chan struct{})}
}

func (b *blockingReadCloser) Read(p []byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *blockingReadCloser) Close() error {
	if !b.once {
		b.once = true
		close(b.closed)
	}
	return nil
}

func TestBuildShimArgs_Order(t *testing.T) {
	m := &Manager{
		bufferSize:      64,
		maxBufBytes:     1 << 20,
		idleTimeout:     5 * time.Minute,
		watchdogTimeout: 30 * time.Second,
	}

	args := m.buildShimArgs("k1", "/sock", "/state", "/cli", "feishu", []string{"--foo", "--bar=baz"}, "/cwd")

	// Expected prefix
	wantPrefix := []string{
		"shim", "run",
		"--key", "k1",
		"--socket", "/sock",
		"--state-file", "/state",
		"--buffer-size", "64",
		"--max-buffer-bytes", "1048576",
		"--idle-timeout", "5m0s",
		"--watchdog-timeout", "30s",
		"--cli-path", "/cli",
		"--cwd", "/cwd",
	}
	for i, w := range wantPrefix {
		if args[i] != w {
			t.Fatalf("args[%d] = %q, want %q\nfull: %v", i, args[i], w, args)
		}
	}

	got := strings.Join(args, " ")
	if !strings.Contains(got, "--backend feishu") {
		t.Errorf("missing backend flag: %v", args)
	}
	if !strings.Contains(got, "--cli-arg --foo") {
		t.Errorf("missing first cli-arg: %v", args)
	}
	if !strings.Contains(got, "--cli-arg --bar=baz") {
		t.Errorf("missing second cli-arg: %v", args)
	}
}

func TestBuildShimArgs_NoBackend_NoCLIArgs(t *testing.T) {
	m := &Manager{bufferSize: 1, maxBufBytes: 2, idleTimeout: time.Second, watchdogTimeout: time.Second}
	args := m.buildShimArgs("k", "/s", "/st", "/c", "", nil, "/w")
	got := strings.Join(args, " ")
	if strings.Contains(got, "--backend") {
		t.Errorf("expected no --backend flag when backend=='', got %v", args)
	}
	if strings.Contains(got, "--cli-arg") {
		t.Errorf("expected no --cli-arg flags when cliArgs is nil, got %v", args)
	}
}

func TestWaitForShimReady_Success(t *testing.T) {
	rc := &nopReadCloser{Reader: bytes.NewBufferString(`{"status":"ready","pid":42,"token":"dG9rZW4="}` + "\n")}
	failed := false
	tok, err := waitForShimReady(context.Background(), rc, 2*time.Second, func() { failed = true })
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if tok != "dG9rZW4=" {
		t.Errorf("token = %q, want %q", tok, "dG9rZW4=")
	}
	if failed {
		t.Errorf("onFail invoked on success path")
	}
}

func TestWaitForShimReady_StatusError(t *testing.T) {
	rc := &nopReadCloser{Reader: bytes.NewBufferString(`{"status":"error","error":"boom"}` + "\n")}
	failed := false
	_, err := waitForShimReady(context.Background(), rc, 2*time.Second, func() { failed = true })
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected boom error, got %v", err)
	}
	if !failed {
		t.Errorf("onFail not invoked on status=error")
	}
}

func TestWaitForShimReady_ParseFailure(t *testing.T) {
	rc := &nopReadCloser{Reader: bytes.NewBufferString("not-json\n")}
	failed := false
	_, err := waitForShimReady(context.Background(), rc, 2*time.Second, func() { failed = true })
	if err == nil || !strings.Contains(err.Error(), "parse ready") {
		t.Fatalf("expected parse ready error, got %v", err)
	}
	if !failed {
		t.Errorf("onFail not invoked on parse failure")
	}
}

func TestWaitForShimReady_EOFBeforeReady(t *testing.T) {
	rc := &nopReadCloser{Reader: bytes.NewBufferString("")}
	failed := false
	_, err := waitForShimReady(context.Background(), rc, 2*time.Second, func() { failed = true })
	if err == nil || !strings.Contains(err.Error(), "exited before ready") {
		t.Fatalf("expected exited-before-ready error, got %v", err)
	}
	if !failed {
		t.Errorf("onFail not invoked on EOF")
	}
}

func TestWaitForShimReady_Timeout(t *testing.T) {
	rc := newBlockingRC()
	failed := false
	start := time.Now()
	_, err := waitForShimReady(context.Background(), rc, 50*time.Millisecond, func() { failed = true })
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "ready timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if !failed {
		t.Errorf("onFail not invoked on timeout")
	}
	if elapsed > time.Second {
		t.Errorf("timeout fired late: %v", elapsed)
	}
}

func TestWaitForShimReady_CtxCancel(t *testing.T) {
	rc := newBlockingRC()
	failed := false
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := waitForShimReady(ctx, rc, 5*time.Second, func() { failed = true })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if !failed {
		t.Errorf("onFail not invoked on ctx cancel")
	}
}
