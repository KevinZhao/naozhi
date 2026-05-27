package cron

import (
	"strings"
	"testing"
)

// TestR250GO15_SpawnCancelInvokedBeforeSendCtx is a structural regression
// test for #1078 / R250-GO-15: executeOpt must explicitly call
// spawnCancel() once GetOrCreate returns successfully, instead of relying
// solely on the function-scoped defer to fire at executeOpt return. The
// defer remains as a safety net, but waiting on it pins the spawn ctx's
// underlying *time.Timer (≤jobTimeout, default 5 minutes) for the entire
// Send window across every in-flight cron run.
//
// Intent on the textual assertion (mirrors sendctx_parent_test.go's
// rationale): there is no ergonomic runtime seam to observe the spawn
// ctx — it's a local in executeOpt and *ManagedSession is concrete, not
// mockable. Pinning the explicit-cancel call is a one-liner regression
// guard for a silent-perf-regression class change.
func TestR250GO15_SpawnCancelInvokedBeforeSendCtx(t *testing.T) {
	t.Parallel()
	src := readSchedulerRunSource(t)

	// 1. The named return — `spawnCancel` (not the legacy `cancel`) — is
	//    declared at the WithTimeout call. Renaming guards against a
	//    future "cleanup" that drops the explicit cancel and leaves the
	//    rebound name behind.
	if !strings.Contains(src, "ctx, spawnCancel := context.WithTimeout(s.stopCtx, jobTimeout)") {
		t.Error("executeOpt must declare the spawn ctx via `ctx, spawnCancel := context.WithTimeout(s.stopCtx, jobTimeout)` — see #1078")
	}

	// 2. The explicit cancel call must appear in source. Combined with
	//    the order check below this pins the early-cancel contract.
	idxExplicit := strings.Index(src, "spawnCancel()")
	if idxExplicit < 0 {
		t.Fatal("executeOpt must call spawnCancel() explicitly after GetOrCreate; defer alone keeps the timer alive for the whole Send window (#1078)")
	}

	// 3. The explicit cancel must come BEFORE sendCtx's WithTimeout call,
	//    confirming the spawn timer is freed before the (long) Send phase
	//    starts — the whole point of the fix. R20260527122801-CR-2 (#1311)
	//    changed the sendCtx parent timeout from `jobTimeout` to a clamped
	//    `sendBudget` value, so the assertion now matches the sendBudget
	//    name; the structural ordering check below is unchanged.
	idxSendCtx := strings.Index(src, "sendCtx, sendCancel := context.WithTimeout(s.stopCtx, sendBudget)")
	if idxSendCtx < 0 {
		t.Fatal("could not locate sendCtx WithTimeout — scheduler_run.go shape changed; revisit this test")
	}
	// There are TWO occurrences of `spawnCancel()` once the fix lands:
	//   - the defer (top of executeOpt)
	//   - the explicit early call after GetOrCreate
	// The early call's body site must precede sendCtx; ensure at least
	// one occurrence sits between the spawn-ctx declaration and sendCtx.
	idxDecl := strings.Index(src, "ctx, spawnCancel := context.WithTimeout(s.stopCtx, jobTimeout)")
	if idxDecl < 0 {
		t.Fatal("spawn ctx declaration site missing — earlier assertion should have caught this")
	}
	region := src[idxDecl:idxSendCtx]
	occurrences := strings.Count(region, "spawnCancel()")
	// One in `defer spawnCancel()` + one explicit call = 2 (or more if a
	// future maintainer adds extras; require at least 2).
	if occurrences < 2 {
		t.Errorf("between spawn-ctx declaration and sendCtx WithTimeout there must be both `defer spawnCancel()` and an explicit `spawnCancel()` call; found %d occurrences", occurrences)
	}
}
