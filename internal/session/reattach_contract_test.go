package session

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestReattachProcessNoCallback_ContractContext is the R31-REL1 pin for the
// human-convention safety constraint documented on ReattachProcessNoCallback:
//
//	SAFETY CONSTRAINT: this function must only be called when Send() cannot be
//	in flight for this session (e.g., during ReconnectShims at startup, or
//	while the session's process is known-dead). If Send() were concurrently
//	executing, the deathReason.Store("") here could silently erase a
//	diagnostic death reason that Send() just set.
//
// The constraint cannot be enforced with a runtime assertion — taking sendMu
// inside ReattachProcessNoCallback would violate the documented sendMu→r.mu
// lock ordering and risk ABBA deadlock. Instead, pin the invariant at source
// level: ReattachProcessNoCallback must have exactly one production caller
// (the shim-reconnect path in router_shim.go since the router-split refactor;
// originally in router.go), and that caller must be preceded by an
// `isAlive()` guard that aborts if the session is still live. Any new
// caller — or any removal of the guard — must be reviewed through this
// audit item.
//
// The test is deliberately strict: it reads the whole session package, lists
// every `ReattachProcessNoCallback(` call site (excluding the definition
// itself), and asserts:
//
//  1. Exactly ONE production call site exists.
//  2. It lives in router_shim.go (not a test or some other file).
//  3. Within 200 bytes above the call, an `isAlive()` guard is present —
//     the canonical shape `currentSess.isAlive()` gates the call on the
//     session being known-dead.
//
// Any future path that uses ReattachProcessNoCallback without the guard
// (e.g. a hot-reload flow, a test harness, a new retry loop) will fail
// this test and the author must either extend the whitelist (with
// justification in this test's comment) or re-route through the
// sendMu-aware ReattachProcess variant.
func TestReattachProcessNoCallback_ContractContext(t *testing.T) {
	t.Parallel()
	// Scan router_shim.go — the only production file that should reference
	// the no-callback variant outside of its own definition in
	// managed_lifecycle.go (moved from managed.go in ARCH-MANAGED-SPLIT).
	// (The router-split refactor moved the shim-reconnect path here; prior
	// to that the call lived in router.go.)
	routerShimSrc, err := os.ReadFile("router_shim.go")
	if err != nil {
		t.Fatalf("read router_shim.go: %v", err)
	}
	managedSrc, err := os.ReadFile("managed_lifecycle.go")
	if err != nil {
		t.Fatalf("read managed.go: %v", err)
	}

	// 1) managed.go must still declare the function (definition, not a
	// call). A typo / rename would break the contract because the
	// docstring lives with the symbol.
	if !regexp.MustCompile(`func \(s \*ManagedSession\) ReattachProcessNoCallback\(`).Match(managedSrc) {
		t.Fatal("ReattachProcessNoCallback is no longer defined in managed_lifecycle.go. " +
			"If it was renamed, update this contract test; if it was removed, " +
			"R31-REL1 is trivially closed but re-review the shim-reconnect path " +
			"for the replacement.")
	}

	// R51-CONCUR-002 (#750): the production shim-reconnect path no longer
	// calls ReattachProcessNoCallback directly — it goes through the
	// TryLock-guarded wrapper tryReattachProcessNoCallback (defined in
	// managed_lifecycle.go alongside the bare variant). That wrapper takes
	// sendMu via TryLock so an in-flight Send unwinding on the just-died
	// process cannot be raced by the storeProcess swap + deathReason reset.
	// So the only callers of the *bare* ReattachProcessNoCallback are inside
	// managed_lifecycle.go (the wrapper). The contract therefore asserts NO
	// other production file calls the bare variant directly.
	callRe := regexp.MustCompile(`\bReattachProcessNoCallback\(`)
	// tryReattachProcessNoCallback( — the guarded wrapper — is the shape the
	// production reconnect path must use. \b would not fire before the lower-
	// case "try" prefix, so match it explicitly.
	tryCallRe := regexp.MustCompile(`\btryReattachProcessNoCallback\(`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir .: %v", err)
	}
	var bareCallSites []string
	var guardedCallSites []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue // this test itself counts toward test-only usage, exempt
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// Strip the guarded-variant occurrences before counting the bare
		// variant so tryReattachProcessNoCallback( does not also match
		// callRe via its embedded ReattachProcessNoCallback( substring.
		bareOnly := tryCallRe.ReplaceAllString(string(data), "")
		if name != "managed_lifecycle.go" && callRe.MatchString(bareOnly) {
			bareCallSites = append(bareCallSites, name)
		}
		if name != "managed_lifecycle.go" && tryCallRe.MatchString(string(data)) {
			guardedCallSites = append(guardedCallSites, name)
		}
	}

	if len(bareCallSites) != 0 {
		t.Errorf("bare ReattachProcessNoCallback production call sites = %v, "+
			"want none. R51-CONCUR-002 (#750): the runtime reconcile path must "+
			"call the sendMu-TryLock-guarded tryReattachProcessNoCallback so an "+
			"in-flight Send on the dying process is not raced. Direct callers of "+
			"the bare variant reopen the logical race.", bareCallSites)
	}
	if len(guardedCallSites) != 1 || guardedCallSites[0] != "router_shim.go" {
		t.Errorf("tryReattachProcessNoCallback production call sites = %v, "+
			"want [router_shim.go]. R31-REL1/R51-CONCUR-002: the sendMu-waiver is "+
			"safe ONLY in the shim-reconnect path where the session is known-dead; "+
			"any new caller must be reviewed through the audit item.", guardedCallSites)
	}

	// The guarded wrapper must actually enforce sendMu via TryLock — a hollow
	// wrapper that just forwards would reopen #750. Pin the TryLock at source.
	if !strings.Contains(string(managedSrc), "sendMu.TryLock()") {
		t.Error("tryReattachProcessNoCallback no longer guards on sendMu.TryLock() — " +
			"R51-CONCUR-002 (#750) regression: the runtime reconcile swap could race " +
			"an in-flight Send unwinding on the just-died process.")
	}

	// 3) Verify the router_shim.go call site is preceded by an isAlive() guard
	// within a short window. We locate the first call and look back ~900
	// characters for `isAlive()` — the typical shape is:
	//    if currentSess != sess || (currentSess != nil && currentSess.isAlive()) {
	//        r.mu.Unlock()
	//        proc.Close()
	//        continue
	//    }
	//    <R51 comment block>
	//    if !sess.tryReattachProcessNoCallback(proc, ...) { ... }
	// The window was widened from 500→900 bytes to accommodate the
	// R51-CONCUR-002 (#750) comment block now sitting between the guard and
	// the call.
	routerStr := string(routerShimSrc)
	callIdx := strings.Index(routerStr, "tryReattachProcessNoCallback(")
	if callIdx < 0 {
		t.Fatal("tryReattachProcessNoCallback call not found in router_shim.go despite the earlier grep")
	}
	windowStart := callIdx - 900
	if windowStart < 0 {
		windowStart = 0
	}
	window := routerStr[windowStart:callIdx]
	if !strings.Contains(window, ".isAlive()") {
		t.Error("ReattachProcessNoCallback call in router_shim.go is no longer preceded " +
			"by an isAlive() guard within ~500 bytes. R31-REL1: the sendMu-waiver " +
			"depends on the session being known-dead before Reattach. Either " +
			"restore the `if currentSess.isAlive() { abort }` gate, or switch to " +
			"ReattachProcess (the sendMu-aware variant) if a live-session reattach " +
			"is actually desired.")
	}

	// 4) The call must be made while holding r.mu. Check that r.mu.Lock()
	// appears within the same window and no intervening r.mu.Unlock() has
	// been inserted between the guard and the Reattach call.
	lastLock := strings.LastIndex(window, "r.mu.Lock()")
	lastUnlock := strings.LastIndex(window, "r.mu.Unlock()")
	if lastLock < 0 {
		t.Error("r.mu.Lock() no longer appears in the 500 bytes preceding " +
			"ReattachProcessNoCallback. R31-REL1: the sendMu-waiver relies on " +
			"the caller holding r.mu so concurrent spawnSession/Reset paths " +
			"cannot racily re-observe the session mid-reattach.")
	}
	if lastUnlock > lastLock && lastUnlock > 0 {
		// An Unlock after the Lock within the window is suspicious — the
		// canonical shape has `r.mu.Unlock()` only inside the abort branch
		// of the guard, not on the fall-through path. Match the exact
		// abort shape: `r.mu.Unlock()\n\t\t\tproc.Close()` must be the only
		// Unlock, and it must be inside an `if` block.
		postLock := window[lastLock:]
		if strings.Count(postLock, "r.mu.Unlock()") > 0 {
			// Permit the canonical abort branch: it ends with `continue`.
			segment := postLock
			if !strings.Contains(segment, "continue") {
				t.Error("An r.mu.Unlock() appears between r.mu.Lock() and " +
					"ReattachProcessNoCallback without a `continue` abort. " +
					"R31-REL1: the Reattach must run while r.mu is held.")
			}
		}
	}
}

// TestTryReattachProcessNoCallback_SendMuGuard pins the R51-CONCUR-002 (#750)
// behaviour: the guarded reattach succeeds only when sendMu is free, and
// returns false (defers the reconnect) when an in-flight Send holds it.
func TestTryReattachProcessNoCallback_SendMuGuard(t *testing.T) {
	t.Parallel()

	s := &ManagedSession{key: "sm750"}
	proc := NewTestProcess()

	// sendMu free → reattach succeeds and the process is published.
	if !s.tryReattachProcessNoCallback(proc, "sid-1") {
		t.Fatal("tryReattachProcessNoCallback returned false with sendMu free")
	}
	if s.loadProcess() != proc {
		t.Error("process was not attached after a successful tryReattach")
	}

	// sendMu held (simulating an in-flight Send unwinding on the dying
	// process) → reattach must defer rather than race the swap.
	s.sendMu.Lock()
	newProc := NewTestProcess()
	if s.tryReattachProcessNoCallback(newProc, "sid-2") {
		s.sendMu.Unlock()
		t.Fatal("tryReattachProcessNoCallback returned true while sendMu was held — " +
			"R51-CONCUR-002 (#750) regression: the swap raced an in-flight Send")
	}
	// The earlier process must remain attached (no partial swap).
	if s.loadProcess() != proc {
		s.sendMu.Unlock()
		t.Error("deferred tryReattach must not have replaced the live process")
	}
	s.sendMu.Unlock()

	// Once sendMu is free again the reattach goes through.
	if !s.tryReattachProcessNoCallback(newProc, "sid-2") {
		t.Fatal("tryReattachProcessNoCallback returned false after sendMu released")
	}
	if s.loadProcess() != newProc {
		t.Error("process was not swapped after sendMu became free")
	}
}
