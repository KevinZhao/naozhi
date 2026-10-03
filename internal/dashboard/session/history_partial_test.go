package session

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
)

func historyIDs(rs []discovery.RecentSession) []string {
	ids := make([]string, len(rs))
	for i, r := range rs {
		ids[i] = r.SessionID
	}
	return ids
}

func TestMergePartialHistory(t *testing.T) {
	now := time.Now().UnixMilli()
	cutoff := now - int64(time.Hour/time.Millisecond)
	prev := []discovery.RecentSession{
		{SessionID: "both", LastActive: now - 50, LastPrompt: "old prompt", Workspace: "/w"},
		{SessionID: "unreached", LastActive: now - 40, LastPrompt: "kept", Workspace: "/w"},
		{SessionID: "now-live", LastActive: now - 30, Workspace: "/w"},
		{SessionID: "aged-out", LastActive: cutoff - 1, Workspace: "/w"},
		{SessionID: "cron", LastActive: now - 20, Workspace: "/w"},
		{SessionID: "sys", LastActive: now - 10, Workspace: "/sys"},
	}
	prevCopy := slices.Clone(prev)
	partial := []discovery.RecentSession{
		{SessionID: "both", LastActive: now, Workspace: "/w"}, // extraction was cancelled
		{SessionID: "new", LastActive: now - 60, LastPrompt: "fresh", Workspace: "/w"},
	}
	exclude := map[string]bool{"now-live": true}
	filter := historyFilter{skipWorkspace: "/sys", skipSessions: map[string]struct{}{"cron": {}}}

	got := mergePartialHistory(partial, prev, exclude, filter, cutoff, 0)

	if want := []string{"both", "unreached", "new"}; !slices.Equal(historyIDs(got), want) {
		t.Fatalf("merged = %v, want %v (newest first; excluded, aged-out and filtered prev entries dropped)", historyIDs(got), want)
	}
	if got[0].LastActive != now {
		t.Errorf("a session in both must take the partial scan's LastActive, got %d", got[0].LastActive)
	}
	if got[0].LastPrompt != "old prompt" {
		t.Errorf("a session whose prompt read was cancelled must borrow the cached prompt, got %q", got[0].LastPrompt)
	}
	if !slices.Equal(prev, prevCopy) {
		t.Error("mergePartialHistory wrote through prev, the slice cache readers alias")
	}

	if limited := mergePartialHistory(partial, prev, exclude, filter, cutoff, 2); !slices.Equal(historyIDs(limited), []string{"both", "unreached"}) {
		t.Errorf("limit 2 = %v, want the two newest", historyIDs(limited))
	}
}

// A scan the deadline cut short must not replace the cached history with the
// sliver it saw (#3141), including right after a retirement invalidated the
// cache; a complete scan still replaces it.
func TestLoadHistorySessionsCtx_PartialScanKeepsCache(t *testing.T) {
	h := &Handlers{deps: Deps{Router: newFakeRouter(), ClaudeDir: t.TempDir()}}
	seed := []discovery.RecentSession{{SessionID: "cached", LastActive: time.Now().UnixMilli()}}
	h.historyCacheMu.Lock()
	h.SetCachedHistoryForTest(seed, time.Now())
	h.historyCacheMu.Unlock()
	h.InvalidateHistoryCache()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := h.loadHistorySessionsCtx(cancelled); !slices.Equal(historyIDs(got), []string{"cached"}) {
		t.Fatalf("cut-short scan returned %v, want the cached session kept", historyIDs(got))
	}
	if got := h.HistorySessionsForTest(); !slices.Equal(historyIDs(got), []string{"cached"}) {
		t.Fatalf("cut-short scan left the cache as %v, want the cached session kept", historyIDs(got))
	}

	if got := h.loadHistorySessionsCtx(context.Background()); len(got) != 0 {
		t.Errorf("complete scan of an empty ~/.claude returned %v, want it to replace the cache", historyIDs(got))
	}
}
