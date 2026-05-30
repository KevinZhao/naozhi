package session

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// fakeHistoryLoader records its invocation and returns canned entries,
// letting session tests exercise the history-load paths without wiring the
// real discovery JSONL chain (ARCH-SESS-1, #458).
type fakeHistoryLoader struct {
	calls   int
	lastIDs []string
	lastCWD string
	entries []cli.EventEntry
}

func (f *fakeHistoryLoader) LoadHistoryChainTail(_ context.Context, _ string, ids []string, cwd string, _ int) []cli.EventEntry {
	f.calls++
	f.lastIDs = ids
	f.lastCWD = cwd
	return f.entries
}

// TestNewRouter_HistoryLoaderDefault verifies that NewRouter installs the
// production discovery-backed loader when the caller leaves cfg.HistoryLoader
// nil, so existing call sites keep working without explicit wiring.
func TestNewRouter_HistoryLoaderDefault(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})
	if r.historyLoader == nil {
		t.Fatal("NewRouter left historyLoader nil; expected discoveryHistoryLoader default")
	}
	if _, ok := r.historyLoader.(discoveryHistoryLoader); !ok {
		t.Fatalf("default historyLoader = %T, want discoveryHistoryLoader", r.historyLoader)
	}
}

// TestNewRouter_HistoryLoaderInjected verifies the injected loader is held
// verbatim and that it satisfies the HistoryLoader contract — the seam that
// lets unit tests stub out the discovery chain.
func TestNewRouter_HistoryLoaderInjected(t *testing.T) {
	t.Parallel()
	want := []cli.EventEntry{{Time: 1, Type: "user", Summary: "hi"}}
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
