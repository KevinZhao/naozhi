// anchor-keep: pins that shim reconnect and drift backfill inject history atomically rather than appending; the double-inject reproduces only under the startup-loader race.
package session

import (
	"os"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestShimReconnect_NoDoubleInjectContract pins the JSONL-load blocks in
// router_shim.go (shim reconnect and drift backfill). NewRouter's startup
// loaders inject into the same session concurrently with ReconnectShimsCtx,
// so every block must pre-check !sess.hasInjectedHistory() to skip the read
// and inject through sess.InjectHistoryIfEmpty, the atomic check-then-act
// (#1812, #3028). A bare sess.InjectHistory or a direct proc.InjectHistory
// appends a second copy of the conversation onto a filled history.
func TestShimReconnect_NoDoubleInjectContract(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("router_shim.go")
	if err != nil {
		t.Fatalf("read router_shim.go: %v", err)
	}
	routerStr := string(src)

	if strings.Contains(routerStr, "sess.InjectHistory(") {
		t.Error("router_shim.go calls sess.InjectHistory, which appends onto " +
			"history a startup loader may already have filled; use " +
			"sess.InjectHistoryIfEmpty")
	}

	// Each block is a claudeDir guard followed within 1500 bytes by the
	// injected r.hist.loader.LoadHistoryChainTail call.
	const guard = "if r.hist.claudeDir != \"\""
	idx := 0
	checked := 0
	for {
		off := strings.Index(routerStr[idx:], guard)
		if off < 0 {
			break
		}
		blockStart := idx + off
		windowEnd := min(blockStart+1500, len(routerStr))
		block := routerStr[blockStart:windowEnd]
		idx = blockStart + len(guard)
		if !strings.Contains(block, "LoadHistoryChainTail") {
			continue
		}
		checked++
		if !strings.Contains(block, "!sess.hasInjectedHistory()") {
			t.Errorf("JSONL-load block %q lacks the !sess.hasInjectedHistory() "+
				"pre-check, so it re-reads JSONL for a session whose history "+
				"is already loaded", firstLine(block))
		}
		if !strings.Contains(block, "sess.InjectHistoryIfEmpty(histEntries)") {
			t.Errorf("JSONL-load block %q does not inject through "+
				"sess.InjectHistoryIfEmpty; a startup loader that fills history "+
				"during the read gets a second copy appended", firstLine(block))
		}
		if strings.Contains(block, "proc.InjectHistory(histEntries)") {
			t.Errorf("JSONL-load block %q calls proc.InjectHistory directly; "+
				"ReattachProcessNoCallback's snapshot then double-fills "+
				"proc.EventLog", firstLine(block))
		}
	}

	if checked != 2 {
		t.Fatalf("router_shim.go has %d JSONL-load blocks of the "+
			"`if r.hist.claudeDir != \"\"` + LoadHistoryChainTail shape, want 2 "+
			"(reconnect and drift backfill); if a load site moved, update this "+
			"contract test to find its new shape", checked)
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
