package shim

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestStartShim_NoReturnBetweenCgroupAndMapInsert is the R49-REL-SHIM-MOVE-
// TO-CGROUP-ROLLBACK pin. The current StartShimWithBackend sequence is:
//
//	handle := m.connect(...)
//	moveToShimsCgroup(ctx, shimPID, cliPID)  // fire-and-forget, no err
//	m.mu.Lock()
//	m.shims[key] = handle                     // handle now reachable
//	m.pendingShims--
//	slotReleased = true
//	m.mu.Unlock()
//
// The "handle → map" transition has no intervening fallible call — any
// error between connect() and the map insert would orphan the shim
// process (alive on the OS, unreachable to naozhi, reaped only by
// shim's 4h idle timeout). Today that invariant holds because
// moveToShimsCgroup is declared as returning nothing and silently
// falls back on failure.
//
// The risk the TODO raises: a future refactor that adds a fallible step
// here (e.g. tightening the cgroup path to return an error, or adding
// a ctx.Err() check) would open the leak window without any test
// catching it. Pin the invariant at source level: within the region
// from the moveToShimsCgroup call to the m.shims[key] = handle line,
// no `return` statement is permitted.
//
// If a legitimate future refactor needs an error path there, the author
// must either (a) reorganise so the map insert happens first, (b) call
// handle.Close() before returning, or (c) extend this test's allowance.
// All three require reviewing through R49-REL-SHIM-MOVE-TO-CGROUP-
// ROLLBACK, which is the intended gate.
func TestStartShim_NoReturnBetweenCgroupAndMapInsert(t *testing.T) {
	src, err := os.ReadFile("manager.go")
	if err != nil {
		t.Fatalf("read manager.go: %v", err)
	}
	body := string(src)

	// Locate the moveToShimsCgroup call inside StartShimWithBackend
	// (the Reconnect path has its own map insert that the separate
	// R49 audit considers; we are only pinning StartShim here).
	startIdx := strings.Index(body, "func (m *Manager) StartShimWithBackend(")
	if startIdx < 0 {
		t.Fatal("StartShimWithBackend is no longer defined in manager.go. " +
			"If it was renamed, update this contract test; if removed, " +
			"R49-REL-SHIM-MOVE-TO-CGROUP-ROLLBACK trivially closes but the " +
			"replacement path must be audited.")
	}

	// Scan forward to the end of the function (next top-level func header).
	rest := body[startIdx:]
	endRel := regexp.MustCompile(`\nfunc `).FindStringIndex(rest[6:])
	var fnBody string
	if endRel != nil {
		fnBody = rest[:6+endRel[0]]
	} else {
		fnBody = rest
	}

	// Find the cgroup call and the map insert within the function body.
	cgroupIdx := strings.Index(fnBody, "moveToShimsCgroup(")
	if cgroupIdx < 0 {
		t.Fatal("moveToShimsCgroup call missing from StartShimWithBackend. " +
			"R49-REL-SHIM-MOVE-TO-CGROUP-ROLLBACK: the contract depends on " +
			"the cgroup call preceding the shims-map insert. If you removed " +
			"the cgroup step, re-audit whether the map insert ordering still " +
			"needs protection.")
	}
	insertIdx := strings.Index(fnBody, "m.shims[key] = handle")
	if insertIdx < 0 {
		t.Fatal("`m.shims[key] = handle` line missing from StartShimWithBackend. " +
			"R49-REL-SHIM-MOVE-TO-CGROUP-ROLLBACK: the contract pins the " +
			"cgroup→insert atomicity window.")
	}
	if insertIdx < cgroupIdx {
		t.Fatal("m.shims[key] = handle appears BEFORE moveToShimsCgroup. " +
			"R49: the expected ordering was cgroup-then-insert (to keep handle " +
			"un-visible if the cgroup step is ever promoted to return an error). " +
			"If the ordering has been reversed deliberately, update this test.")
	}

	// Extract the window and ensure no bare `return` statement lives in it.
	// `return nil, ...` / `return x, err` / bare `return` are all rejected —
	// any exit path between these two source lines would skip the map insert
	// and leak the handle.
	window := fnBody[cgroupIdx:insertIdx]
	returnRe := regexp.MustCompile(`(?m)^\s*return\b`)
	if loc := returnRe.FindStringIndex(window); loc != nil {
		t.Errorf("`return` statement detected between moveToShimsCgroup and "+
			"m.shims[key] = handle (offset %d within the window). R49-REL-"+
			"SHIM-MOVE-TO-CGROUP-ROLLBACK: any early exit in this region "+
			"orphans the shim process — it stays alive on the OS with its "+
			"PID reachable only through Discover-on-next-startup (4 h idle "+
			"timeout in the worst case). Either call handle.Close() before "+
			"return, or reorder so the map insert happens first and then "+
			"handle.Close() only on later failure.", loc[0])
	}

	// Defensive check: the region must also not contain a panicking helper
	// (the only way to skip the return scan above). Current code has no
	// panic invocations here, so a new one would be suspicious.
	if strings.Contains(window, "panic(") {
		t.Error("`panic(...)` invocation detected in the cgroup→insert window. " +
			"Same R49 concern as the return check — a panic escaping to the " +
			"deferred slot-release bypasses the map insert.")
	}
}
