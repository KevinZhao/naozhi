// anchor-keep: pins the per-RPC ctx choice (appCtx vs connCtx) as a source matrix; each wrong choice is a subtle lifetime bug that only reproduces on reconnect races.
package upstream

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// RNEW-008 (#424): handleRequest accepts both appCtx and connCtx with
// different cancellation contracts per RPC branch — takeover honours appCtx
// ("survives reconnect"), send hands its turn to the node's Orchestrator and
// keeps connCtx only for the session spawn before it answers. The rule lives
// in a godoc matrix on handleRequest, but doc drifts from code silently. A
// future RPC author copy-pasting a goroutine into the wrong branch
// reintroduces the orphan-goroutine risk the issue flags. These guards fail
// the build when the wiring no longer matches the documented contract.
//
// We inspect source text rather than running a live session because the
// takeover goroutine calls into real CLI-backed *ManagedSession work that
// cannot be exercised without spawning a claude child process. The
// circuit-breaker tests in this package already rely on the same
// source-inspection technique (connector_circuit_breaker_test.go).

// rpcSrc reads connector_rpc.go once for the matrix guards.
func rpcSrc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("connector_rpc.go")
	if err != nil {
		t.Fatalf("read connector_rpc.go: %v", err)
	}
	return string(b)
}

// caseBody returns the source text of the `case "<method>":` block in
// handleRequest, from the case label up to (but not including) the next
// top-level `case ` / closing of the switch. Good enough to scope the
// ctx-usage assertions to a single RPC branch.
func caseBody(t *testing.T, src, method string) string {
	t.Helper()
	start := strings.Index(src, `case "`+method+`":`)
	if start < 0 {
		t.Fatalf("case %q not found in connector_rpc.go", method)
	}
	rest := src[start+len(`case "`+method+`":`):]
	// Next case label (at any indentation) bounds this branch.
	if next := regexp.MustCompile(`\n\tcase "`).FindStringIndex(rest); next != nil {
		rest = rest[:next[0]]
	}
	return rest
}

// TestHandleRequest_CtxMatrix_SendSubmitsOnConnCtx asserts the `send`
// branch hands the message to the turn pipeline on connCtx and starts no
// turn of its own: no goroutine, no direct session send, no appCtx.
func TestHandleRequest_CtxMatrix_SendSubmitsOnConnCtx(t *testing.T) {
	body := caseBody(t, rpcSrc(t), "send")
	if !strings.Contains(body, "c.turns.SubmitRelayed(connCtx") {
		t.Error(`send branch must call c.turns.SubmitRelayed(connCtx, ...) — the turn runs on the node's Orchestrator`)
	}
	for _, banned := range []string{"go func", ".Send(", "wg.Add"} {
		if strings.Contains(body, banned) {
			t.Errorf("send branch contains %q — it must not run a turn of its own", banned)
		}
	}
	// Ban appCtx as a call argument (doc mentions are fine).
	if regexp.MustCompile(`\(appCtx[,)]`).MatchString(body) {
		t.Error(`send branch must NOT pass appCtx into any call — the spawn before the answer is connection-scoped`)
	}
}

// TestHandleRequest_CtxMatrix_TakeoverUsesAppCtx asserts the takeover
// goroutine is wired to appCtx (so a transient WS drop does not abort
// cleanup already in progress) and does not use connCtx — the documented
// "survives across reconnect" contract.
func TestHandleRequest_CtxMatrix_TakeoverUsesAppCtx(t *testing.T) {
	body := caseBody(t, rpcSrc(t), "takeover")
	if !strings.Contains(body, "WaitAndCleanup(appCtx") {
		t.Error(`takeover branch must call discovery.WaitAndCleanup(appCtx, ...) — takeover is app-scoped per the RNEW-008 matrix`)
	}
	if !strings.Contains(body, "Takeover(appCtx") {
		t.Error(`takeover branch must call router.Takeover(appCtx, ...) — cleanup must survive a reconnect`)
	}
	// Ban connCtx as a CALL ARGUMENT (e.g. "Foo(connCtx" or "(connCtx,").
	// A bare doc mention ("appCtx outlives connCtx") is fine; what we
	// guard against is the goroutine threading connCtx into real work,
	// which would abort cleanup on a transient WS drop.
	if regexp.MustCompile(`\(connCtx[,)]`).MatchString(body) {
		t.Error(`takeover branch must NOT pass connCtx into any call — cleanup must outlive the WS connection`)
	}
}
