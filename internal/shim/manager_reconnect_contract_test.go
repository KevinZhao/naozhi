package shim

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestReconnect_SwapClosesOldHandleContract is the R49-REL-SHIM-MANAGER-
// RECONNECT-CONCUR pin for the "swap must close old handle" invariant in
// Manager.Reconnect. When a concurrent Reconnect for the same key races,
// the late winner's `m.shims[key] = handle` would otherwise leak the
// prior fd + bufio buffers.
//
// The broader race — early winner's handle being closed while a caller
// already attached it to a Process — is out of scope for a source-level
// test (it needs a runtime singleflight to solve). But pinning the
// swap-close-old behaviour itself is cheap:
//
//  1. `oldHandle := m.shims[key]` MUST be captured under m.mu BEFORE
//     the write.
//  2. `m.shims[key] = handle` MUST happen under m.mu.
//  3. `oldHandle.Close()` MUST happen OUTSIDE m.mu (Close performs
//     network I/O; holding m.mu across it would serialise every other
//     Manager mutation behind a possibly-slow Close).
//  4. The RACE CONTRACT godoc tag must remain in place so future
//     reviewers follow the chain back to this audit item.
//
// Any refactor that drops the swap-capture (leaking old fd), swaps
// without closing (leaking handle), or moves the Close inside the lock
// (regressing mutation throughput) fails CI.
func TestReconnect_SwapClosesOldHandleContract(t *testing.T) {
	src, err := os.ReadFile("manager.go")
	if err != nil {
		t.Fatalf("read manager.go: %v", err)
	}
	body := string(src)

	// 4) RACE CONTRACT tripwire comment must survive.
	if !regexp.MustCompile(`RACE CONTRACT \(R49-REL-SHIM-MANAGER-RECONNECT-CONCUR\)`).MatchString(body) {
		t.Error("Reconnect no longer carries the RACE CONTRACT godoc. " +
			"R49-REL-SHIM-MANAGER-RECONNECT-CONCUR: the comment is the only " +
			"in-code anchor linking the swap-close-old behaviour to this audit " +
			"item. Keep the R49-REL-SHIM-MANAGER-RECONNECT-CONCUR tag so grep " +
			"still lands here.")
	}

	// Locate the Reconnect function body for the structural checks.
	startIdx := strings.Index(body, "func (m *Manager) Reconnect(")
	if startIdx < 0 {
		t.Fatal("Manager.Reconnect is no longer defined. If renamed, update " +
			"this test; if removed, re-audit the replacement path for the " +
			"same swap-close-old invariant.")
	}
	rest := body[startIdx:]
	endRel := regexp.MustCompile(`\nfunc `).FindStringIndex(rest[6:])
	var fnBody string
	if endRel != nil {
		fnBody = rest[:6+endRel[0]]
	} else {
		fnBody = rest
	}

	// 1) Must capture old handle under m.mu before writing. The accepted
	// shape is:
	//    m.mu.Lock()
	//    ...
	//    oldHandle := m.shims[key]
	//    m.shims[key] = handle
	//    ...
	//    m.mu.Unlock()
	captureRe := regexp.MustCompile(`oldHandle\s*:=\s*m\.shims\[key\]`)
	if !captureRe.MatchString(fnBody) {
		t.Error("Reconnect no longer captures `oldHandle := m.shims[key]` " +
			"before the swap. Without this, the Close step below has nothing " +
			"to close and a concurrent Reconnect leaks the prior handle's fd " +
			"(+ bufio buffers, etc). R49-REL-SHIM-MANAGER-RECONNECT-CONCUR.")
	}

	// 2) `m.shims[key] = handle` exists.
	if !strings.Contains(fnBody, "m.shims[key] = handle") {
		t.Error("Reconnect no longer writes `m.shims[key] = handle`. The swap " +
			"must run under m.mu so concurrent readers see an atomic replace. " +
			"R49-REL-SHIM-MANAGER-RECONNECT-CONCUR.")
	}

	// 3) oldHandle.Close() happens outside m.mu (appears AFTER the first
	// m.mu.Unlock() following the swap). Heuristic: the Close call must
	// come after the m.mu.Unlock() nearest to the swap, and not inside a
	// `defer m.mu.Unlock()` block.
	swapIdx := strings.Index(fnBody, "m.shims[key] = handle")
	// Find the nearest m.mu.Unlock() after the swap.
	postSwap := fnBody[swapIdx:]
	unlockIdx := strings.Index(postSwap, "m.mu.Unlock()")
	if unlockIdx < 0 {
		t.Error("No m.mu.Unlock() appears after `m.shims[key] = handle` in " +
			"Reconnect. R49-REL-SHIM-MANAGER-RECONNECT-CONCUR: the Close " +
			"step must run outside m.mu so I/O does not block every other " +
			"Manager mutation; if you reorganised to `defer m.mu.Unlock()`, " +
			"update this test but verify Close still runs after the defer.")
	}
	afterUnlock := postSwap[unlockIdx:]
	closeIdx := strings.Index(afterUnlock, "oldHandle.Close()")
	if closeIdx < 0 {
		t.Error("oldHandle.Close() no longer appears after m.mu.Unlock() in " +
			"Reconnect. R49-REL-SHIM-MANAGER-RECONNECT-CONCUR: dropping the " +
			"Close step leaks the fd on every concurrent Reconnect; moving " +
			"it before Unlock serialises every other Manager mutation behind " +
			"network I/O.")
	}

	// 4) Sanity: the Close must be guarded by a nil check so the no-prior-
	// handle path does not NPE.
	// We accept either `if oldHandle != nil { oldHandle.Close() }` or the
	// short-form `if oldHandle != nil` followed by close on the next line.
	nilCheckRe := regexp.MustCompile(`if\s+oldHandle\s*!=\s*nil\s*\{[\s\S]{0,80}oldHandle\.Close\(\)`)
	if !nilCheckRe.MatchString(fnBody) {
		t.Error("Reconnect's `oldHandle.Close()` is no longer guarded by " +
			"`if oldHandle != nil`. First-time swap (empty m.shims[key]) " +
			"would panic on the nil dereference.")
	}
}
