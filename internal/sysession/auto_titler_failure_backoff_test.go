package sysession

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// perPromptRunner answers a two-line (rejected) title for prompts that
// mention bad, a valid one otherwise.
type perPromptRunner struct{ badCalls, goodCalls atomic.Int32 }

func (r *perPromptRunner) Run(_ context.Context, prompt string) (string, error) {
	if strings.Contains(prompt, "bad-session") {
		r.badCalls.Add(1)
		return "标题\n这是解释", nil
	}
	r.goodCalls.Add(1)
	return "旧会话标题", nil
}

// TestAutoTitler_FailedRenameBacksOff: a session whose rename fails must be
// gated like a renamed one, so the next tick neither spends another call on
// it nor keeps the older untitled session from its turn.
func TestAutoTitler_FailedRenameBacksOff(t *testing.T) {
	t.Parallel()
	now := time.Now().UnixMilli()
	bad := session.SessionSnapshot{Key: "feishu:direct:u1:general", MessageCount: 3, LastPrompt: "bad-session", LastActive: now}
	old := session.SessionSnapshot{Key: "feishu:direct:u2:general", MessageCount: 3, LastPrompt: "older", LastActive: now - 60_000}
	router := newSnapshotFakeRouter([]session.SessionSnapshot{bad, old})
	runner := &perPromptRunner{}
	a, err := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
	if err != nil {
		t.Fatalf("newAutoTitler: %v", err)
	}

	if _, err := a.Tick(context.Background()); err == nil {
		t.Fatal("first tick: want the rejected title's error")
	}
	if _, err := a.Tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if n := runner.badCalls.Load(); n != 1 {
		t.Errorf("failing session tried %d times in two ticks, want 1", n)
	}
	if got := router.label(old.Key); got != "旧会话标题" {
		t.Errorf("older session label = %q, want it titled on the second tick", got)
	}
}

// flakyRunner fails its first call with an upstream (non-validation) error
// and answers a valid title afterwards.
type flakyRunner struct{ calls atomic.Int32 }

func (r *flakyRunner) Run(_ context.Context, _ string) (string, error) {
	if r.calls.Add(1) == 1 {
		return "", errors.New("bedrock: 503 service unavailable")
	}
	return "恢复后的标题", nil
}

// TestAutoTitler_UpstreamFailureDoesNotHoldSession: a Runner failure is not
// about the session, so it must not be recorded as a rename. An idle,
// never-titled session is titled by the next tick once the upstream
// recovers, without waiting for minUserTurns new user turns.
func TestAutoTitler_UpstreamFailureDoesNotHoldSession(t *testing.T) {
	t.Parallel()
	idle := session.SessionSnapshot{Key: "feishu:direct:u1:general", MessageCount: 2, LastPrompt: "idle", LastActive: time.Now().UnixMilli()}
	router := newSnapshotFakeRouter([]session.SessionSnapshot{idle})
	runner := &flakyRunner{}
	a, err := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
	if err != nil {
		t.Fatalf("newAutoTitler: %v", err)
	}

	if _, err := a.Tick(context.Background()); err == nil {
		t.Fatal("first tick: want the upstream error")
	}
	if _, err := a.Tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := router.label(idle.Key); got != "恢复后的标题" {
		t.Errorf("label = %q after the upstream recovered, want the session titled (runner calls %d)", got, runner.calls.Load())
	}
}
