package server

import (
	"os"
	"regexp"
	"testing"
)

// TestResubscribeEvents_OldUnsubReleasedOutsideMu is the H8 (Round 163)
// pin for the "oldUnsub() must be invoked after releasing h.mu" contract
// in Hub.resubscribeEvents.
//
// Why this matters: the global lock order is h.mu → EventLog.subMu
// (see TestHubShutdown_LockOrderInvariant for the shutdown direction).
// Calling an unsub closure while holding h.mu is technically legal
// today — unsub ends up taking subMu, which is strictly downstream.
// But if a future refactor adds anything to the unsub chain that
// takes a lock which can be acquired under subMu (for example, an
// audit or observability lock), the current pattern would silently
// introduce the reverse order.
//
// The defensive pattern is: snapshot the old unsub pointer under
// h.mu, swap in the new entry, release h.mu, then invoke the
// closure. This source-level test pins the pattern so a future
// "simplification" that moves oldUnsub() back under the lock fails
// the build.
func TestResubscribeEvents_OldUnsubReleasedOutsideMu(t *testing.T) {
	src, err := os.ReadFile("wshub.go")
	if err != nil {
		t.Fatalf("read wshub.go: %v", err)
	}
	text := string(src)

	// Invariant 1: the swap arm MUST follow the documented
	// capture-swap-unlock-then-invoke pattern (whitespace-tolerant).
	swapPattern := regexp.MustCompile(`(?s)oldUnsub\s*:=\s*c\.subscriptions\[key\]\s*\n\s*c\.subscriptions\[key\]\s*=\s*unsub\s*\n\s*h\.mu\.Unlock\(\)\s*\n\s*if\s+oldUnsub\s*!=\s*nil\s*\{\s*\n\s*oldUnsub\(\)\s*\n\s*\}`)
	if !swapPattern.MatchString(text) {
		t.Error("H8 (Round 163): resubscribeEvents swap arm no longer matches " +
			"the capture-swap-unlock-then-invoke pattern. It must read:\n" +
			"    oldUnsub := c.subscriptions[key]\n" +
			"    c.subscriptions[key] = unsub\n" +
			"    h.mu.Unlock()\n" +
			"    if oldUnsub != nil { oldUnsub() }\n" +
			"Calling oldUnsub() under h.mu reintroduces the latent reverse " +
			"lock-order risk (h.mu held while downstream locks are taken). " +
			"If you genuinely need to serialise the unsub with other h.mu " +
			"protected state, document the new invariant and update this " +
			"test — do not just move the call back under the lock.")
	}

	// Invariant 2: the timeout arm MUST also capture the stale unsub
	// under h.mu, release, then invoke.
	timeoutPattern := regexp.MustCompile(`(?s)var\s+staleUnsub\s+func\(\).*?delete\(c\.subscriptions,\s*key\)\s*\n.*?h\.mu\.Unlock\(\)\s*\n\s*if\s+staleUnsub\s*!=\s*nil\s*\{\s*\n\s*staleUnsub\(\)\s*\n\s*\}`)
	if !timeoutPattern.MatchString(text) {
		t.Error("H8 (Round 163): resubscribeEvents timeout cleanup no longer " +
			"matches the capture-swap-unlock-then-invoke pattern for the " +
			"staleUnsub path. Both the successful swap arm and the timeout " +
			"arm must release h.mu before invoking the captured unsub.")
	}

	// Invariant 3: the body MUST NOT contain the legacy inline pattern
	// where oldUnsub() is invoked *before* h.mu.Unlock() in the swap
	// arm. Matching specifically the pre-H8 shape.
	legacyPattern := regexp.MustCompile(`(?s)if\s+oldUnsub,\s*exists\s*:=\s*c\.subscriptions\[key\];\s*exists\s*\{\s*\n?\s*oldUnsub\(\)\s*\n?\s*\}\s*\n\s*c\.subscriptions\[key\]\s*=\s*unsub\s*\n\s*h\.mu\.Unlock\(\)`)
	if legacyPattern.MatchString(text) {
		t.Error("H8 (Round 163): legacy pattern `if oldUnsub, exists ...; exists { oldUnsub() }` " +
			"before h.mu.Unlock() has reappeared. This is exactly the shape " +
			"H8 removed. Restore the capture-then-unlock form described above.")
	}

	// Invariant 4: the H8 contract comment must still reference the
	// Round number so future readers can locate this test.
	if !regexp.MustCompile(`H8 \(Round 163\)`).MatchString(text) {
		t.Error("H8 anchor comment missing. Keep `H8 (Round 163)` searchable " +
			"in wshub.go so future reviewers land on this contract.")
	}
}
