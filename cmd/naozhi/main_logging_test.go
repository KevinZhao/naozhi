package main

import (
	"context"
	"log/slog"
	"testing"

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
