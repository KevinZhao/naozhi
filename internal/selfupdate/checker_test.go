package selfupdate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// withStubbedLatest swaps latestRelease for a test and restores it after.
// Do NOT use t.Parallel with this (package convention).
func withStubbedLatest(t *testing.T, fn func(context.Context) (*Release, error)) {
	t.Helper()
	orig := latestRelease
	latestRelease = fn
	t.Cleanup(func() { latestRelease = orig })
}

func TestParseMode(t *testing.T) {
	cases := map[string]Mode{
		"notify":   ModeNotify,
		"download": ModeDownload,
		"auto":     ModeAuto,
		"":         ModeDownload, // unknown → safe configured default
		"garbage":  ModeDownload,
	}
	for in, want := range cases {
		if got := ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewChecker_NilOnBadInterval(t *testing.T) {
	if c := NewChecker(CheckerConfig{Interval: 0}); c != nil {
		t.Error("NewChecker with zero interval should return nil")
	}
	if c := NewChecker(CheckerConfig{Interval: -time.Hour}); c != nil {
		t.Error("NewChecker with negative interval should return nil")
	}
	if c := NewChecker(CheckerConfig{Interval: time.Hour}); c == nil {
		t.Error("NewChecker with valid interval should not be nil")
	}
}

func TestNewChecker_DefaultsModeToDownload(t *testing.T) {
	c := NewChecker(CheckerConfig{Interval: time.Hour})
	if c.cfg.Mode != ModeDownload {
		t.Errorf("default mode = %q, want %q", c.cfg.Mode, ModeDownload)
	}
}

// notify mode: a newer release fires exactly one notice and does NOT touch
// the binary (no download stub needed — doInstall is never reached).
func TestCheckOnce_NotifyMode(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return &Release{Tag: "v9.9.9"}, nil
	})
	var got []string
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeNotify,
		Interval:       time.Hour,
		Notify:         func(s string) { got = append(got, s) },
	})
	c.checkOnce(context.Background())
	if len(got) != 1 {
		t.Fatalf("expected 1 notice, got %d: %v", len(got), got)
	}
}

// Up-to-date: no notice, no work.
func TestCheckOnce_UpToDate(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return &Release{Tag: "v1.0.0"}, nil
	})
	var notices int
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeNotify,
		Interval:       time.Hour,
		Notify:         func(string) { notices++ },
	})
	c.checkOnce(context.Background())
	if notices != 0 {
		t.Errorf("up-to-date should emit no notice, got %d", notices)
	}
}

// dev build self-skips before any network call.
func TestCheckOnce_DevBuildSkips(t *testing.T) {
	called := false
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		called = true
		return &Release{Tag: "v9.9.9"}, nil
	})
	c := NewChecker(CheckerConfig{
		CurrentVersion: "dev",
		Mode:           ModeNotify,
		Interval:       time.Hour,
		Notify:         func(string) {},
	})
	c.checkOnce(context.Background())
	if called {
		t.Error("dev build must not query for releases")
	}
}

// A failed release lookup logs+swallows: no notice, no panic.
func TestCheckOnce_LookupFailureSwallowed(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return nil, errors.New("network down")
	})
	var notices int
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeNotify,
		Interval:       time.Hour,
		Notify:         func(string) { notices++ },
	})
	c.checkOnce(context.Background()) // must not panic
	if notices != 0 {
		t.Errorf("lookup failure should emit no notice, got %d", notices)
	}
}

// Run honors ctx cancellation promptly and stops the loop.
func TestRun_StopsOnContextCancel(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return &Release{Tag: "v1.0.0"}, nil
	})
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeNotify,
		Interval:       time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after ctx cancel")
	}
}

// nil Checker.Run is a safe no-op (NewChecker returned nil path).
func TestRun_NilChecker(t *testing.T) {
	var c *Checker
	done := make(chan struct{})
	go func() { c.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("nil Checker.Run should return immediately")
	}
}

// notify with a nil NotifyFunc must not panic.
func TestNotify_NilFuncSafe(t *testing.T) {
	c := NewChecker(CheckerConfig{Interval: time.Hour})
	c.notify("anything") // no panic, no-op
}

// concurrent notice delivery from the closure is data-race safe under -race
// when the caller's NotifyFunc guards its own state.
func TestNotify_Concurrent(t *testing.T) {
	var mu sync.Mutex
	var n int
	c := NewChecker(CheckerConfig{
		Interval: time.Hour,
		Notify: func(string) {
			mu.Lock()
			n++
			mu.Unlock()
		},
	})
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.notify("x") }()
	}
	wg.Wait()
	if n != 10 {
		t.Errorf("expected 10 notices, got %d", n)
	}
}
