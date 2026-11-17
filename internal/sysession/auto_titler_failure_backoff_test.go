package sysession

import (
	"context"
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
