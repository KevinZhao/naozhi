package sysession

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

const restartTestKey = "feishu:direct:u1:general"

func newRestartTestTitler(t *testing.T, snaps []session.SessionSnapshot) (*autoTitler, *snapshotFakeRouter, *fakeRunner) {
	t.Helper()
	router := newSnapshotFakeRouter(snaps)
	runner := &fakeRunner{resp: "新标题"}
	d, err := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
	if err != nil {
		t.Fatalf("newAutoTitler: %v", err)
	}
	return d.(*autoTitler), router, runner
}

func tickOK(t *testing.T, a *autoTitler) TickReport {
	t.Helper()
	rep, err := a.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick err = %v", err)
	}
	return rep
}

// setHighwater replaces the single highwater entry for restartTestKey.
func setHighwater(a *autoTitler, hw autoTitlerHighwater) {
	a.highwater.Store(&map[string]autoTitlerHighwater{restartTestKey: hw})
}

// A fresh process (empty highwater) must not re-title the auto titles a
// previous process wrote: it seeds them at the current turn count, and the
// next re-title needs minUserTurns new turns as usual.
func TestAutoTitler_RestoredAutoTitleSeededNotRenamed(t *testing.T) {
	t.Parallel()
	a, router, runner := newRestartTestTitler(t, []session.SessionSnapshot{{
		Key: restartTestKey, MessageCount: 50,
		UserLabel: "旧标题", LabelOrigin: "auto", LastPrompt: "继续",
	}})

	rep := tickOK(t, a)
	if runner.calls.Load() != 0 || rep.Acted != 0 {
		t.Fatalf("restored auto title was renamed: calls=%d report=%+v", runner.calls.Load(), rep)
	}
	if rep.Skipped["restored_auto_title"] != 1 {
		t.Fatalf("Skipped = %v, want restored_auto_title=1", rep.Skipped)
	}
	hw, ok := (*a.highwater.Load())[restartTestKey]
	if !ok || hw.lastRenameAtTurn != 50 || time.Since(hw.lastRenamedAt).Abs() > time.Second {
		t.Fatalf("highwater = %+v (present=%v), want seeded now at turn 50", hw, ok)
	}

	// The seed starts the rename interval: enough new turns, still throttled.
	router.snaps[0].MessageCount = 53
	if rep := tickOK(t, a); rep.Acted != 0 || rep.Skipped["min_rename_interval"] != 1 {
		t.Fatalf("53 turns inside interval: report=%+v, want min_rename_interval", rep)
	}

	// Past the interval with two new turns: still throttled.
	setHighwater(a, autoTitlerHighwater{lastRenamedAt: time.Now().Add(-time.Hour), lastRenameAtTurn: 50})
	router.snaps[0].MessageCount = 52
	if rep := tickOK(t, a); rep.Acted != 0 || rep.Skipped["no_new_turns"] != 1 {
		t.Fatalf("52 turns: report=%+v, want no_new_turns", rep)
	}

	router.snaps[0].MessageCount = 53
	if rep := tickOK(t, a); rep.Acted != 1 || runner.calls.Load() != 1 {
		t.Fatalf("53 turns: report=%+v calls=%d, want one rename", rep, runner.calls.Load())
	}
}

// A turn count below the recorded rename point (idle eviction swaps the
// since-spawn count for the windowed persisted one) rebaselines the
// highwater, so the session re-titles after minUserTurns turns from the new
// count instead of waiting to climb back past the old one.
func TestAutoTitler_TurnCountRegressionRebaselines(t *testing.T) {
	t.Parallel()
	a, router, runner := newRestartTestTitler(t, []session.SessionSnapshot{{
		Key: restartTestKey, MessageCount: 60,
		UserLabel: "旧标题", LabelOrigin: "auto", LastPrompt: "继续",
	}})
	renamedAt := time.Now().Add(-10 * time.Minute)
	setHighwater(a, autoTitlerHighwater{lastRenamedAt: renamedAt, lastRenameAtTurn: 300})

	rep := tickOK(t, a)
	if rep.Acted != 0 || rep.Skipped["no_new_turns"] != 1 {
		t.Fatalf("regressed count: report=%+v, want no_new_turns", rep)
	}
	hw := (*a.highwater.Load())[restartTestKey]
	if hw.lastRenameAtTurn != 60 || !hw.lastRenamedAt.Equal(renamedAt) {
		t.Fatalf("highwater = %+v, want turn 60 with lastRenamedAt kept", hw)
	}

	router.snaps[0].MessageCount = 63
	if rep := tickOK(t, a); rep.Acted != 1 || runner.calls.Load() != 1 {
		t.Fatalf("63 turns: report=%+v calls=%d, want one rename", rep, runner.calls.Load())
	}
}

// Seeds are committed on an earlyStop tick (candidate cap reached) and on
// the ctx-cancel return, and don't use up candidate slots.
func TestAutoTitler_SeedsSurviveEarlyStopAndCancel(t *testing.T) {
	t.Parallel()
	snaps := []session.SessionSnapshot{{
		Key: restartTestKey, MessageCount: 7,
		UserLabel: "旧标题", LabelOrigin: "auto", LastPrompt: "继续",
	}}
	// batchPerTick=1 caps candidates at 4; five fresh sessions trip earlyStop.
	for i := range 5 {
		snaps = append(snaps, session.SessionSnapshot{
			Key: "feishu:direct:" + fmtKey(i) + ":general", MessageCount: 2, LastPrompt: "问题",
		})
	}

	t.Run("earlyStop", func(t *testing.T) {
		t.Parallel()
		a, _, _ := newRestartTestTitler(t, snaps)
		rep := tickOK(t, a)
		if rep.Examined != 5 || rep.Acted != 1 {
			t.Fatalf("report=%+v, want 5 examined (seed + 4 candidates) and 1 rename", rep)
		}
		if hw, ok := (*a.highwater.Load())[restartTestKey]; !ok || hw.lastRenameAtTurn != 7 {
			t.Fatalf("seed lost on earlyStop: %+v (present=%v)", hw, ok)
		}
	})
	t.Run("ctx cancelled", func(t *testing.T) {
		t.Parallel()
		a, _, runner := newRestartTestTitler(t, snaps)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := a.Tick(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Tick err = %v, want context.Canceled", err)
		}
		if runner.calls.Load() != 0 {
			t.Fatalf("runner called %d times after cancel", runner.calls.Load())
		}
		if hw, ok := (*a.highwater.Load())[restartTestKey]; !ok || hw.lastRenameAtTurn != 7 {
			t.Fatalf("seed lost on ctx cancel: %+v (present=%v)", hw, ok)
		}
	})
}

// The seed branch is for existing auto titles only: an untitled session
// (including one whose auto title was cleared) still gets its first title on
// the first tick, however many turns it already has (#2271).
func TestAutoTitler_UntitledSessionTitledOnFirstTick(t *testing.T) {
	t.Parallel()
	for _, origin := range []string{"", "auto"} {
		a, _, runner := newRestartTestTitler(t, []session.SessionSnapshot{{
			Key: restartTestKey, MessageCount: 50, LabelOrigin: origin, LastPrompt: "帮我查日志",
		}})
		if rep := tickOK(t, a); rep.Acted != 1 || runner.calls.Load() != 1 {
			t.Fatalf("origin %q: report=%+v calls=%d, want first title", origin, rep, runner.calls.Load())
		}
	}
}
