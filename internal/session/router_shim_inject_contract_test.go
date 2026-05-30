package session

import (
	"os"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestShimReconnect_NoDoubleInjectContract pins the R231-CQ-1 fix at source
// level. The bug: the shim-reconnect path used to call
// `proc.InjectHistory(histEntries)` directly while persistedHistory may have
// already been populated by tier1 (NewRouter startup goroutine via
// s.InjectHistory). The subsequent ReattachProcessNoCallback's
// attachProcessAndSnapshotPersisted then snapshotted persistedHistory and
// fed it to proc again — every overlapping entry landed in proc.EventLog
// twice and EventLog persisted both copies.
//
// The fix routes JSONL load through sess.InjectHistory and gates it behind
// !sess.hasInjectedHistory() so persistedHistory remains the single source
// of truth and seededLen accounting prevents duplicate forwarding.
//
// This test asserts the source pattern at the load site so a future
// refactor that reverts to the direct proc.InjectHistory call (or drops
// the hasInjectedHistory gate) fails the contract.
func TestShimReconnect_NoDoubleInjectContract(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("router_shim.go")
	if err != nil {
		t.Fatalf("read router_shim.go: %v", err)
	}
	routerStr := string(src)

	// Locate the JSONL-load block: the load site is uniquely identified by
	// the LoadHistoryChainTail + claudeDir guard pair. Walk every guard
	// in the file and verify the fix invariants near each one. Since #458
	// the load goes through the injected r.historyLoader.LoadHistoryChainTail
	// rather than discovery.LoadHistoryChainTailCtx; matching the shorter
	// "LoadHistoryChainTail" substring keeps the pin stable across both.
	const guard = "if r.claudeDir != \"\""
	idx := 0
	checked := 0
	for {
		off := strings.Index(routerStr[idx:], guard)
		if off < 0 {
			break
		}
		blockStart := idx + off
		// Scan forward at most 1500 bytes for the matching LoadHistoryChainTail
		// — this is the JSONL-load shape we care about.
		windowEnd := blockStart + 1500
		if windowEnd > len(routerStr) {
			windowEnd = len(routerStr)
		}
		block := routerStr[blockStart:windowEnd]
		if !strings.Contains(block, "LoadHistoryChainTail") {
			idx = blockStart + len(guard)
			continue
		}
		checked++

		// Invariant 1: the load is gated by !sess.hasInjectedHistory() (or
		// equivalent skip when persistedHistory is non-empty). Without
		// this gate, tier1 + JSONL stack and persistedHistory grows
		// duplicates internally.
		if !strings.Contains(block, "hasInjectedHistory()") {
			// Drift branch (line ~283) is allowed without the gate ONLY
			// because it runs BEFORE any tier1 fires for that key (the
			// shimManagedKeys claim suppresses NewRouter's deferred load).
			// Identify the drift branch by its distinctive
			// "drifted shim: backfilled JSONL history" log key.
			if strings.Contains(block, "drifted shim") {
				idx = blockStart + len(guard)
				continue
			}
			t.Errorf("R231-CQ-1: JSONL-load block at offset %d lacks "+
				"!sess.hasInjectedHistory() gate. Without it tier1 "+
				"(NewRouter startup) and the shim-reconnect path race "+
				"to populate persistedHistory and produce duplicate "+
				"entries after the post-Reattach snapshot copies the "+
				"already-injected prefix into proc.EventLog a second "+
				"time. Block: %q", blockStart, firstLine(block))
		}

		// Invariant 2: the load result must be funnelled through
		// sess.InjectHistory (NOT proc.InjectHistory). The direct
		// proc.InjectHistory call bypasses persistedHistory and
		// guarantees a duplicate when ReattachProcessNoCallback fires
		// the snapshot copy below.
		if strings.Contains(block, "proc.InjectHistory(histEntries)") {
			t.Errorf("R231-CQ-1: JSONL-load block at offset %d still "+
				"calls proc.InjectHistory(histEntries) directly. Route "+
				"through sess.InjectHistory so persistedHistory tracks "+
				"the load and the upcoming ReattachProcessNoCallback "+
				"snapshot does not double-fill proc.EventLog.", blockStart)
		}
		idx = blockStart + len(guard)
	}

	if checked == 0 {
		t.Fatal("router_shim.go has no JSONL-load block matching the " +
			"`if r.claudeDir != \"\"` + LoadHistoryChainTail shape. " +
			"If the load site moved, update this contract test to find " +
			"its new shape.")
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
	tier1 := []cli.EventEntry{
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
