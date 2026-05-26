package shim

// R246-CR-005 / #740 P0 subset: awaitReady was extracted from
// StartShimWithBackend so the spawn function reads as a 7-step
// lifecycle script rather than 67 lines of inlined goroutine + timer
// + 3-way select. These tests pin the four observable outcomes
// (success / shim error / timeout / ctx cancel) so future refactors
// of the helper cannot silently regress one of the failure modes.

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// pipeWriter wraps an os.Pipe write end and exposes the read end as
// io.ReadCloser, mirroring the cmd.StdoutPipe() shape that
// StartShimWithBackend hands to awaitReady.
func makePipe(t *testing.T) (io.ReadCloser, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = w.Close()
		_ = r.Close()
	})
	return r, w
}

func TestAwaitReady_Success(t *testing.T) {
	t.Parallel()
	r, w := makePipe(t)

	go func() {
		_, _ = w.Write([]byte(`{"status":"ready","pid":1234,"token":"YWJjZA=="}` + "\n"))
		_ = w.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tok, err := awaitReady(ctx, r, 2*time.Second)
	if err != nil {
		t.Fatalf("awaitReady err: %v", err)
	}
	if tok != "YWJjZA==" {
		t.Fatalf("token mismatch: got %q want %q", tok, "YWJjZA==")
	}
}

func TestAwaitReady_ShimReportsError(t *testing.T) {
	t.Parallel()
	r, w := makePipe(t)
	go func() {
		_, _ = w.Write([]byte(`{"status":"error","error":"cli not found"}` + "\n"))
		_ = w.Close()
	}()

	_, err := awaitReady(context.Background(), r, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "cli not found") {
		t.Fatalf("expected wrapped shim error, got %v", err)
	}
}

func TestAwaitReady_UnexpectedStatus(t *testing.T) {
	t.Parallel()
	r, w := makePipe(t)
	go func() {
		_, _ = w.Write([]byte(`{"status":"warming","pid":1234}` + "\n"))
		_ = w.Close()
	}()

	_, err := awaitReady(context.Background(), r, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "unexpected status") {
		t.Fatalf("expected unexpected-status error, got %v", err)
	}
}

func TestAwaitReady_MalformedJSON(t *testing.T) {
	t.Parallel()
	r, w := makePipe(t)
	go func() {
		_, _ = w.Write([]byte("not-json\n"))
		_ = w.Close()
	}()

	_, err := awaitReady(context.Background(), r, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "parse ready") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestAwaitReady_ShimExitsBeforeReady(t *testing.T) {
	t.Parallel()
	r, w := makePipe(t)
	// Close immediately with no bytes written.
	_ = w.Close()

	_, err := awaitReady(context.Background(), r, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "exited before ready") {
		t.Fatalf("expected exited-before-ready error, got %v", err)
	}
}

func TestAwaitReady_Timeout(t *testing.T) {
	t.Parallel()
	r, _ := makePipe(t) // never writes

	start := time.Now()
	_, err := awaitReady(context.Background(), r, 50*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "ready timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("timeout took %v, expected ~50ms", elapsed)
	}
}

func TestAwaitReady_ContextCancel(t *testing.T) {
	t.Parallel()
	r, _ := makePipe(t) // never writes

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := awaitReady(ctx, r, 5*time.Second)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed >= 1*time.Second {
		t.Fatalf("ctx cancel returned in %v, expected ~20ms", elapsed)
	}
}
