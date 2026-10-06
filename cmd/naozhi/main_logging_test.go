package main

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/config"
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

// TestNewLogHandler_FormatSelection verifies that "text" selects a
// TextHandler and any other value (including the default "json") selects a
// JSONHandler, and that the resolved level gates Enabled(). R237-ARCH-8.
func TestNewLogHandler_FormatSelection(t *testing.T) {
	t.Parallel()

	text := newLogHandler(nil, &config.Config{Log: config.LogConfig{Format: "text", Level: "debug"}}, nil)
	if _, ok := text.(*slog.TextHandler); !ok {
		t.Fatalf("format=text: got %T, want *slog.TextHandler", text)
	}
	if !text.Enabled(context.Background(), slog.LevelDebug) {
		t.Errorf("level=debug handler should enable Debug")
	}

	js := newLogHandler(nil, &config.Config{Log: config.LogConfig{Format: "json", Level: "warn"}}, nil)
	if _, ok := js.(*slog.JSONHandler); !ok {
		t.Fatalf("format=json: got %T, want *slog.JSONHandler", js)
	}
	if js.Enabled(context.Background(), slog.LevelInfo) {
		t.Errorf("level=warn handler must not enable Info")
	}

	// Empty format defaults to JSON (matches the legacy else-branch).
	def := newLogHandler(nil, &config.Config{Log: config.LogConfig{Format: ""}}, nil)
	if _, ok := def.(*slog.JSONHandler); !ok {
		t.Fatalf("format empty: got %T, want *slog.JSONHandler (default)", def)
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

// config.Load logs the dashboard-token warning under Go's default stderr
// handler; main must log it again once setupLogging has installed the
// configured one, or the service log never carries it.
func TestMain_WarnsDashboardTokenAfterSetupLogging(t *testing.T) {
	t.Parallel()
	src := readSrc(t, "main.go")
	setup := strings.Index(src, ":= setupLogging(cfg)\n")
	warn := strings.Index(src, "config.WarnDashboardToken(cfg.Server.DashboardToken)")
	if setup < 0 {
		t.Fatal("main.go: setupLogging(cfg) call not found")
	}
	if warn < setup {
		t.Errorf("main.go: config.WarnDashboardToken must be called after setupLogging(cfg) (setup at %d, warn at %d)", setup, warn)
	}
}
