package cron

import (
	"strings"
	"testing"
)

// TestR250GO15_SpawnCancelInvokedBeforeSendCtx is a structural regression
// test for #1078 / R250-GO-15: the spawn ctx must be explicitly cancelled
// once GetOrCreate returns successfully, instead of relying solely on the
// function-scoped defer to fire at executeOpt return. The defer remains as a
// safety net, but waiting on it pins the spawn ctx's underlying *time.Timer
// (≤jobTimeout, default 5 minutes) for the entire Send window across every
// in-flight cron run.
//
// RNEW-003 (#423): the GetOrCreate spawn phase moved into executeGetSession,
// so the explicit cancel now fires there (`a.spawnCancel()` — the cancel func
// is threaded in via getSessionArgs.spawnCancel) while executeOpt keeps the
// `defer spawnCancel()` safety net. This test therefore asserts the contract
// across both functions in scheduler_run.go: executeOpt still owns the named
// `spawnCancel` + defer + threads it into the helper, and the helper invokes
// it explicitly before the success return (which precedes sendCtx).
//
// Intent on the textual assertion (mirrors sendctx_parent_test.go's
// rationale): there is no ergonomic runtime seam to observe the spawn ctx —
// it's a local and *ManagedSession is concrete, not mockable. Pinning the
// explicit-cancel call is a one-liner regression guard for a silent-perf-
// regression class change.
func TestR250GO15_SpawnCancelInvokedBeforeSendCtx(t *testing.T) {
	t.Parallel()
	src := readSchedulerRunSource(t)

	// 1. executeOpt declares the spawn ctx with the named `spawnCancel`
	//    cancel func + the defer safety net, and threads the cancel func
	//    into the spawn-phase helper. Renaming or dropping any of these
	//    guards against a "cleanup" that loses the early-cancel contract.
	if !strings.Contains(src, "ctx, spawnCancel := context.WithTimeout(s.stopCtx, jobTimeout)") {
		t.Error("executeOpt must declare the spawn ctx via `ctx, spawnCancel := context.WithTimeout(s.stopCtx, jobTimeout)` — see #1078")
	}
	if !strings.Contains(src, "defer spawnCancel()") {
		t.Error("executeOpt must keep `defer spawnCancel()` as the safety net — see #1078")
	}
	if !strings.Contains(src, "spawnCancel: spawnCancel,") {
		t.Error("executeOpt must thread the spawn cancel func into executeGetSession via getSessionArgs.spawnCancel — see #423/#1078")
	}

	// 2. The explicit early cancel now lives in executeGetSession as
	//    `a.spawnCancel()`. It must fire BEFORE the success return so the
	//    spawn timer frees before the (long) Send phase begins — the whole
	//    point of the #1078 fix.
	idxExplicit := strings.Index(src, "a.spawnCancel()")
	if idxExplicit < 0 {
		t.Fatal("executeGetSession must call a.spawnCancel() explicitly after a successful GetOrCreate; defer alone keeps the timer alive for the whole Send window (#1078)")
	}
	idxSuccessReturn := strings.Index(src, "return sess, spawnStart, false")
	if idxSuccessReturn < 0 {
		t.Fatal("could not locate executeGetSession success return — scheduler_run.go shape changed; revisit this test")
	}
	if idxExplicit >= idxSuccessReturn {
		t.Error("a.spawnCancel() must precede executeGetSession's success return so the spawn timer is freed before Send (#1078)")
	}

	// 3. In executeOpt, sendCtx is set up only after the spawn phase returns
	//    (R20260527122801-CR-2 / #1311 clamped its parent timeout to
	//    sendBudget). Confirm the helper invocation precedes sendCtx so the
	//    early cancel ordering is preserved end-to-end.
	idxHelperCall := strings.Index(src, "s.executeGetSession(getSessionArgs{")
	if idxHelperCall < 0 {
		t.Fatal("executeOpt must delegate the spawn phase to executeGetSession — see #423")
	}
	// Phase C (#734/#945): sendCtx creation lives in execSend (defined after
	// executeOpt), so source order "helper call before sendCtx" still holds.
	idxSendCtx := strings.Index(src, "sendCtx, sendCancel := context.WithTimeout(s.stopCtx, a.sendBudget)")
	if idxSendCtx < 0 {
		t.Fatal("could not locate sendCtx WithTimeout — scheduler_run.go shape changed; revisit this test")
	}
	if idxHelperCall >= idxSendCtx {
		t.Error("executeGetSession (which fires the early spawnCancel) must be invoked before sendCtx is created (#1078)")
	}
}
