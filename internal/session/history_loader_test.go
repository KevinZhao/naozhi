package session

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/history"
)

// fakeHistoryLoader records its invocation and returns canned entries,
// letting session tests exercise the history-load paths without wiring the
// real discovery JSONL chain (ARCH-SESS-1, #458).
type fakeHistoryLoader struct {
	calls   int
	lastIDs []string
	lastCWD string
	entries []clievent.EventEntry
}

func (f *fakeHistoryLoader) LoadHistoryChainTail(_ context.Context, _ string, ids []string, cwd string, _ int) []clievent.EventEntry {
	f.calls++
	f.lastIDs = ids
	f.lastCWD = cwd
	return f.entries
}

// TestNewRouter_HistoryLoaderDefault verifies that NewRouter installs the
// production claude-factory-backed loader when the caller leaves
// cfg.HistoryLoader nil, so existing call sites keep working without
// explicit wiring.
func TestNewRouter_HistoryLoaderDefault(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})
	if r.historyLoader == nil {
		t.Fatal("NewRouter left historyLoader nil; expected claudeTranscriptLoader default")
	}
	if _, ok := r.historyLoader.(claudeTranscriptLoader); !ok {
		t.Fatalf("default historyLoader = %T, want claudeTranscriptLoader", r.historyLoader)
	}
}

// TestClaudeTranscriptLoader_InjectedPick verifies the loader resolves the
// "claude" factory through an injected pick (not the global registry),
// passes beforeMS==0 and limit through unchanged, and hands the factory a
// SessionView/Wiring built from the loader's own arguments.
func TestClaudeTranscriptLoader_InjectedPick(t *testing.T) {
	t.Parallel()
	var gotID string
	var gotCWD string
	var gotIDs []string
	var gotClaudeDir string
	var gotBeforeMS int64
	var gotLimit int

	factory := history.FactoryFn(func(s history.SessionView, deps history.Wiring) history.Source {
		gotCWD = s.Workspace()
		gotIDs = s.SnapshotChainIDs()
		gotClaudeDir = deps.ClaudeDir
		return fakeSourceFn(func(_ context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
			gotBeforeMS = beforeMS
			gotLimit = limit
			return []clievent.EventEntry{{Time: 1, Type: "user", Summary: "hi"}}, nil
		})
	})
	loader := claudeTranscriptLoader{pick: func(id string) history.FactoryFn {
		gotID = id
		return factory
	}}

	got := loader.LoadHistoryChainTail(context.Background(), "/claude", []string{"sid"}, "/ws", 10)

	if gotID != "claude" {
		t.Errorf("pick called with id=%q, want \"claude\"", gotID)
	}
	if gotCWD != "/ws" || len(gotIDs) != 1 || gotIDs[0] != "sid" {
		t.Errorf("factory saw Workspace=%q SnapshotChainIDs=%v, want /ws [sid]", gotCWD, gotIDs)
	}
	if gotClaudeDir != "/claude" {
		t.Errorf("factory saw Wiring.ClaudeDir=%q, want /claude", gotClaudeDir)
	}
	if gotBeforeMS != 0 {
		t.Errorf("LoadBefore beforeMS=%d, want 0", gotBeforeMS)
	}
	if gotLimit != 10 {
		t.Errorf("LoadBefore limit=%d, want 10 (unchanged)", gotLimit)
	}
	if len(got) != 1 || got[0].Summary != "hi" {
		t.Fatalf("got %v, want 1 entry 'hi'", got)
	}
}

// fakeSourceFn adapts a func to history.Source for TestClaudeTranscriptLoader_InjectedPick.
type fakeSourceFn func(ctx context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error)

func (f fakeSourceFn) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
	return f(ctx, beforeMS, limit)
}

// TestClaudeTranscriptLoader_MissingFactory verifies a nil pick result (no
// "claude" factory registered) degrades to an empty load rather than a panic.
func TestClaudeTranscriptLoader_MissingFactory(t *testing.T) {
	t.Parallel()
	loader := claudeTranscriptLoader{pick: func(string) history.FactoryFn { return nil }}
	got := loader.LoadHistoryChainTail(context.Background(), "/claude", []string{"sid"}, "/ws", 10)
	if got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

// TestNewRouter_HistoryLoaderInjected verifies the injected loader is held
// verbatim and that it satisfies the HistoryLoader contract — the seam that
// lets unit tests stub out the discovery chain.
func TestNewRouter_HistoryLoaderInjected(t *testing.T) {
	t.Parallel()
	want := []clievent.EventEntry{{Time: 1, Type: "user", Summary: "hi"}}
	fake := &fakeHistoryLoader{entries: want}
	r := NewRouter(RouterConfig{HistoryLoader: fake})
	if r.historyLoader != fake {
		t.Fatalf("historyLoader = %p, want injected fake %p", r.historyLoader, fake)
	}

	got := r.historyLoader.LoadHistoryChainTail(context.Background(), "/claude", []string{"sid"}, "/ws", 10)
	if fake.calls != 1 {
		t.Fatalf("loader call count = %d, want 1", fake.calls)
	}
	if fake.lastCWD != "/ws" || len(fake.lastIDs) != 1 || fake.lastIDs[0] != "sid" {
		t.Fatalf("loader received ids=%v cwd=%q, want [sid] /ws", fake.lastIDs, fake.lastCWD)
	}
	if len(got) != 1 || got[0].Summary != "hi" {
		t.Fatalf("loader returned %v, want %v", got, want)
	}
}
