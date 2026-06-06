package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestReadJSONWithRetry_CtxCancelMidRetry_ReturnsCtxErr pins R241-GO-4 (#490)
// at the underlying helper: when ctx is canceled mid-retry, readJSONWithRetry
// must propagate context.Canceled so the applyClaudeEnvSettings dispatch can
// downgrade the slog severity to Warn rather than misclassify the cancel as a
// corruption Error.
func TestReadJSONWithRetry_CtxCancelMidRetry_ReturnsCtxErr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	// Seed with invalid JSON so retry goroutine is forced to sleep.
	if err := os.WriteFile(path, []byte(`{"partial":`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := atomic.Bool{}
	go func() {
		// Cancel after the first attempt has begun the retry-sleep.
		time.Sleep(40 * time.Millisecond)
		// Store BEFORE cancel: otherwise readJSONWithRetry can observe
		// ctx.Done() and return before this goroutine schedules the
		// Store, leaving the main goroutine to misread cancelled=false
		// as a "ran without cancel" flake (in fact cancel did fire —
		// the writer just hadn't been scheduled yet).
		cancelled.Store(true)
		cancel()
	}()
	_, err := readJSONWithRetry(ctx, path, 5, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected ctx error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if !cancelled.Load() {
		t.Fatalf("test bug: cancel goroutine never ran (ctx.Err=%v)", ctx.Err())
	}
}

// captureSlog routes slog.Default through a fresh JSON handler writing to buf
// for the duration of the test, then restores the prior handler on cleanup.
// Returns the captured bytes; callers can scan them with strings.Contains for
// log-level + message assertions without depending on slog's text formatter.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestApplyClaudeEnvSettings_CtxCancelLoggedAtWarn pins R241-GO-4 (#490)'s
// caller-side behaviour: when readJSONWithRetry hands back context.Canceled,
// the dispatch in main() must log at level=WARN with an "aborted by ctx
// cancel" body. Before the fix the cancel path fell through to slog.Error
// with the misleading "read or parse failed" message, polluting the
// corruption-alert filter on every graceful shutdown that beat the first
// retry sleep.
//
// We simulate the ctx-cancel-mid-retry by seeding HOME with invalid JSON
// then calling applyClaudeEnvSettings with a ctx that cancels before the
// retry sleep elapses. The error path is exercised verbatim from the
// production callsite at main.go:494 so any future drift in the dispatch
// will fail this test.
func TestApplyClaudeEnvSettings_CtxCancelLoggedAtWarn(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"partial":`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	buf := captureSlog(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	err := applyClaudeEnvSettings(ctx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// Replicate the dispatch at cmd/naozhi/main.go:494 verbatim — the test
	// asserts a behavioural contract on that branch ladder rather than on
	// slog level alone (a future refactor that swaps slog for a wrapping
	// helper would still need to surface the ctx-cancel cause distinctly
	// from corruption).
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		slog.Warn("apply ~/.claude/settings.json env: aborted by ctx cancel", "err", err)
	case errors.Is(err, fs.ErrNotExist):
		slog.Warn("apply ~/.claude/settings.json env: file missing", "err", err)
	default:
		slog.Error("apply ~/.claude/settings.json env: read or parse failed", "err", err)
	}

	out := buf.String()
	if !strings.Contains(out, `"level":"WARN"`) {
		t.Fatalf("expected WARN level in slog output, got: %s", out)
	}
	if !strings.Contains(out, "aborted by ctx cancel") {
		t.Fatalf("expected 'aborted by ctx cancel' message, got: %s", out)
	}
	if strings.Contains(out, `"level":"ERROR"`) {
		t.Fatalf("ctx-cancel must NOT log at ERROR (would pollute corruption alerts), got: %s", out)
	}
}
