package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/ctxutil"
)

// TestResolveLogLevel covers the config.Log.Level → slog.Level mapping
// extracted from main() in R237-ARCH-8 (#590). Unknown / empty fall back
// to Info, matching the legacy switch default.
func TestResolveLogLevel(t *testing.T) {
	t.Parallel()
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"warn":    slog.LevelWarn,
		"error":   slog.LevelError,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"bogus":   slog.LevelInfo,
		"DEBUG":   slog.LevelInfo, // case-sensitive, mirrors legacy switch
		"warning": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := resolveLogLevel(in); got != want {
			t.Errorf("resolveLogLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestNewLogHandler_JSONAndLevel verifies the handler is a ctx-correlating
// JSONHandler and that the resolved level gates Enabled(). R237-ARCH-8.
func TestNewLogHandler_JSONAndLevel(t *testing.T) {
	t.Parallel()

	h := newLogHandler(nil, &config.Config{Log: config.LogConfig{Level: "warn"}}, nil)
	// Every handler is wrapped for ctx correlation (#3436).
	w, ok := h.(*ctxutil.Handler)
	if !ok {
		t.Fatalf("got %T, want the *ctxutil.Handler wrapper", h)
	}
	if _, ok := w.Unwrap().(*slog.JSONHandler); !ok {
		t.Fatalf("got %T, want *slog.JSONHandler", w.Unwrap())
	}
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Errorf("level=warn handler must not enable Info")
	}
	if !newLogHandler(nil, &config.Config{Log: config.LogConfig{Level: "debug"}}, nil).Enabled(context.Background(), slog.LevelDebug) {
		t.Errorf("level=debug handler should enable Debug")
	}
}

// TestStartWatchdogLoop_StopsOnCtxCancel ensures the heartbeat goroutine
// returns when its context is cancelled, and that a nil HealthCheck func is
// tolerated (no panic). The 30s tick means no heartbeat fires inside the
// test window — we only assert clean shutdown. R237-ARCH-8 (#590).
func TestStartWatchdogLoop_StopsOnCtxCancel(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	startWatchdogLoop(ctx, func() bool { calls.Add(1); return true })
	cancel()
	// Give the goroutine a moment to observe ctx.Done(); no assertion on
	// goroutine count (the runtime offers no portable hook), but the cancel
	// path must not deadlock or panic.
	time.Sleep(20 * time.Millisecond)

	// nil HealthCheck must not panic on construction.
	ctx2, cancel2 := context.WithCancel(context.Background())
	startWatchdogLoop(ctx2, nil)
	cancel2()
	time.Sleep(20 * time.Millisecond)
}
