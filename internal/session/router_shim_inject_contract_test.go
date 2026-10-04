// anchor-keep: pins that every startup and shim history inject goes event log first and inject-if-empty; the double-inject and Claude-only view reproduce only under the startup-loader race.
package session

import (
	"os"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestShimReconnect_NoDoubleInjectContract pins how restored history reaches
// a session. NewRouter's startup loaders inject concurrently with
// ReconnectShimsCtx, so router_shim.go's two sites (reconnect and drift
// backfill) pre-check !sess.hasInjectedHistory() to skip the read and go
// through injectRestoredHistory, and router_restore.go reads each source once
// and injects only through InjectHistoryIfEmpty (#1812, #3028). A plain
// InjectHistory appends a second copy onto a filled history; a direct JSONL
// read beside the helper can win the race with the Claude-only view.
func TestShimReconnect_NoDoubleInjectContract(t *testing.T) {
	t.Parallel()
	read := func(name string) string {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(src)
	}
	shimSrc := read("router_shim.go")
	for _, banned := range []string{".InjectHistory", ".LoadHistoryChainTail(", ".LoadLatest("} {
		if strings.Contains(shimSrc, banned) {
			t.Errorf("router_shim.go names %s; restore history through "+
				"r.hist.injectRestoredHistory so the event log is tried first "+
				"and the inject stays atomic", banned)
		}
	}
	const call = "r.hist.injectRestoredHistory("
	sites := strings.Split(shimSrc, call)
	if len(sites)-1 != 2 {
		t.Fatalf("router_shim.go calls injectRestoredHistory %d times, want 2 "+
			"(reconnect and drift backfill); if a site moved, update this test",
			len(sites)-1)
	}
	for i, before := range sites[:len(sites)-1] {
		if !strings.Contains(before[max(0, len(before)-600):], "if !sess.hasInjectedHistory() {") {
			t.Errorf("injectRestoredHistory site %d lacks the !sess.hasInjectedHistory() "+
				"pre-check, so it re-reads history a session already holds", i+1)
		}
	}

	restoreSrc := read("router_restore.go")
	for name, want := range map[string]int{
		"InjectHistoryIfEmpty(": 2,
		"LoadLatest(":           1,
		"LoadHistoryChainTail(": 1,
		"InjectHistory(":        0,
	} {
		if got := strings.Count(restoreSrc, name); got != want {
			t.Errorf("router_restore.go has %d %s, want %d: the event-log and "+
				"JSONL helpers are the only readers and inject only if empty",
				got, name, want)
		}
	}
}

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
