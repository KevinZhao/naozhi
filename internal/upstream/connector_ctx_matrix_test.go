package upstream

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// RNEW-008 (#424): handleRequest accepts both appCtx and connCtx with
// different cancellation contracts per RPC branch — send honours connCtx
// ("dies with the WS"), takeover honours appCtx ("survives reconnect").
// The rule lives in a godoc matrix on handleRequest, but doc drifts from
// code silently. A future RPC author copy-pasting the send goroutine into
// a takeover-shaped branch (or vice versa) reintroduces the orphan-goroutine
// risk the issue flags. These guards fail the build when the wiring no
// longer matches the documented contract.
//
// We inspect source text rather than running a live session because the
// send/takeover goroutines call into real CLI-backed *ManagedSession work
// that cannot be exercised without spawning a claude child process. The
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

// TestHandleRequest_CtxMatrix_DocPresent pins the godoc rule matrix so a
// future "tidy the comments" refactor cannot delete the only written
// record of the appCtx/connCtx contract.
func TestHandleRequest_CtxMatrix_DocPresent(t *testing.T) {
	src := rpcSrc(t)
	for _, want := range []string{
		"Context selection matrix",
		"connCtx",
		"appCtx",
		"connection-scoped",
		"app-scoped",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("handleRequest godoc missing %q — the ctx-selection matrix (RNEW-008) must stay documented", want)
		}
	}
}

// TestHandleRequest_CtxMatrix_SendUsesConnCtx asserts the async `send`
// goroutine is wired to connCtx (so a relay disconnect cancels in-flight
// sends) and never reaches for appCtx — the documented "dies with the WS"
// contract.
func TestHandleRequest_CtxMatrix_SendUsesConnCtx(t *testing.T) {
	body := caseBody(t, rpcSrc(t), "send")
	if !strings.Contains(body, "sess.Send(connCtx") {
		t.Error(`send branch must call sess.Send(connCtx, ...) — send is connection-scoped per the RNEW-008 matrix`)
	}
	// Ban appCtx as a call argument (doc mentions are fine).
	if regexp.MustCompile(`\(appCtx[,)]`).MatchString(body) {
		t.Error(`send branch must NOT pass appCtx into any call — sends must die with the WS connection, not outlive it (orphan-goroutine risk)`)
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
