package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestShimReconnect_HasInjectedHistorySkipsLoad is the behavioural pin: when
// persistedHistory is already populated (tier1 won the race), a fresh
// reconnect should observe `hasInjectedHistory()=true` and the JSONL-load
// block becomes a no-op. The downstream ReattachProcessNoCallback still
// snapshots persistedHistory into the new proc, so the dashboard sees the
// history exactly once.
//
// This is a unit-level companion to the contract test above: it doesn't
// drive the full ReconnectShims body (which requires shim plumbing) but
// pins the post-fix invariant — ReattachProcessNoCallback alone, against a
// pre-populated session, leaves proc.EventLog with one copy of each entry.
func TestShimReconnect_HasInjectedHistorySkipsLoad(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "feishu:direct:alice:general"}

	// Tier1 populates persistedHistory before the shim-reconnect runs.
	tier1 := []clievent.EventEntry{
		{Time: 1000, Type: "user", Summary: "tier1-msg-a"},
		{Time: 2000, Type: "text", Summary: "tier1-reply-a"},
		{Time: 3000, Type: "user", Summary: "tier1-msg-b"},
	}
	s.InjectHistory(tier1)

	if !s.hasInjectedHistory() {
		t.Fatal("preconditions: hasInjectedHistory() should report true after tier1 inject")
	}

	// Post-fix shim-reconnect path skips the JSONL load (because
	// hasInjectedHistory() is true) and proceeds straight to
	// ReattachProcessNoCallback.
	proc := NewTestProcess()
	s.ReattachProcessNoCallback(proc, "session-uuid")

	// proc.EventLog should now hold the persistedHistory snapshot exactly
	// once — no doubles from a redundant JSONL re-inject.
	got := s.EventEntries()
	if len(got) != len(tier1) {
		t.Fatalf("EventEntries() len=%d want %d (post-fix: tier1 + skip-load = "+
			"single snapshot copy)", len(got), len(tier1))
	}
	seen := make(map[string]int, len(got))
	for _, e := range got {
		seen[e.Summary]++
	}
	for sum, n := range seen {
		if n != 1 {
			t.Errorf("summary %q appeared %d times in proc.EventLog; "+
				"R231-CQ-1 invariant: each entry exactly once", sum, n)
		}
	}
}
