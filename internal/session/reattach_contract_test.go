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
//	in flight for this session. If Send() were concurrently executing, the
//	deathReason.Store("") here could silently erase a diagnostic death reason
//	that Send() just set, and the process swap would publish a fresh process
//	from under the in-flight Send.
//
// The constraint cannot be enforced from inside ReattachProcessNoCallback —
// taking sendMu there would violate the documented sendMu→r.mu lock ordering
// and risk ABBA deadlock. Instead, pin the invariant at source level: the
// single production caller (the shim-reconnect path in router_shim.go) must
// (a) be preceded by an `isAlive()` guard that aborts if the session is still
// live, and (b) hold sess.sendMu across the call — acquired via TryLock BEFORE
// r.mu so the (sendMu → r.mu) order matches Send() with no ABBA risk (#750).
// Any new caller — or any removal of either guard — must be reviewed through
// this audit item.
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

	// 2) Count call sites. Any non-test file other than managed.go (the
	// definition) is considered a production call site.
	callRe := regexp.MustCompile(`\bReattachProcessNoCallback\(`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir .: %v", err)
	}
	var prodCallSites []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue // this test itself counts toward test-only usage, exempt
		}
		if name == "managed_lifecycle.go" {
			continue // definition file (moved from managed.go in ARCH-MANAGED-SPLIT Phase 2)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if callRe.Match(data) {
			prodCallSites = append(prodCallSites, name)
		}
	}

	if len(prodCallSites) != 1 || prodCallSites[0] != "router_shim.go" {
		t.Errorf("ReattachProcessNoCallback production call sites = %v, "+
			"want [router_shim.go]. R31-REL1: the sendMu-waiver is safe ONLY in the "+
			"shim-reconnect path where the session is known-dead; any new caller "+
			"must be reviewed through the audit item. If intentional, add the "+
			"new file to a whitelist here with justification.", prodCallSites)
	}

	// 3) Verify the router_shim.go call site is preceded by an isAlive() guard
	// within a short window. We locate the first call and look back ~500
	// characters for `isAlive()` — the typical shape is:
	//    if currentSess != sess || (currentSess != nil && currentSess.isAlive()) {
	//        r.mu.Unlock()
	//        proc.Close()
	//        continue
	//    }
	//    sess.ReattachProcessNoCallback(proc, ...)
	routerStr := string(routerShimSrc)
	callIdx := strings.Index(routerStr, "ReattachProcessNoCallback(")
	if callIdx < 0 {
		t.Fatal("ReattachProcessNoCallback call not found in router_shim.go despite the earlier grep")
	}
	// Window widened to 1600 bytes: the #750 fix added a multi-line comment
	// block plus the sendMu.TryLock() guard above the isAlive()/r.mu.Lock()
	// pair, pushing them further from the call site than the original 500.
	windowStart := callIdx - 1600
	if windowStart < 0 {
		windowStart = 0
	}
	window := routerStr[windowStart:callIdx]
	if !strings.Contains(window, ".isAlive()") {
		t.Error("ReattachProcessNoCallback call in router_shim.go is no longer preceded " +
			"by an isAlive() guard within ~1600 bytes. R31-REL1: the sendMu-waiver " +
			"depends on the session being known-dead before Reattach. Either " +
			"restore the `if currentSess.isAlive() { abort }` gate, or switch to " +
			"ReattachProcess (the sendMu-aware variant) if a live-session reattach " +
			"is actually desired.")
	}

	// #750: the call must also be guarded by sess.sendMu so an in-flight Send
	// cannot race the deathReason/process swap. The canonical shape acquires
	// it via TryLock before r.mu.Lock(). Removing this re-opens the logical
	// race the issue describes.
	if !strings.Contains(window, "sendMu.TryLock()") {
		t.Error("ReattachProcessNoCallback call in router_shim.go is no longer preceded " +
			"by a sess.sendMu.TryLock() guard within ~1600 bytes. R51-CONCUR-002 (#750): " +
			"the reattach mutates deathReason and the active process pointer; without " +
			"holding sendMu an in-flight Send racing on the dying process can clobber " +
			"those writes. Re-add the TryLock guard (acquired BEFORE r.mu to keep the " +
			"sendMu→r.mu order) or switch to the sendMu-aware ReattachProcess variant.")
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
