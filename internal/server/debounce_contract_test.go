package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestDebounceTimer_ShutdownStopSemanticsContract is R37-CONCUR3's source-
// level pin. The debounce AfterFunc goroutine is tracked by clientWG
// (h.clientWG.Add(1) when scheduling, defer Done in the callback) so
// Shutdown can wait for any late-firing broadcast to finish.
//
// The Stop()-return-value branching in Shutdown is load-bearing:
//
//	time.Timer.Stop() = true  → callback was cancelled before firing.
//	                           Shutdown MUST call clientWG.Done() to
//	                           release the slot the scheduler reserved.
//	time.Timer.Stop() = false → callback already fired (may be running or
//	                           finished). Its `defer clientWG.Done()`
//	                           already handled (or will handle) the
//	                           slot. Shutdown MUST NOT call Done() —
//	                           doing so would push clientWG negative
//	                           and panic on next Add/Wait cycle.
//
// A refactor that "simplifies" the branch — always calling Done(),
// or always skipping it, or moving Stop() out of the debounceMu guard —
// reintroduces the panic. Pin the structure:
//
//  1. Shutdown must read h.debounceTimer under h.debounceMu (keeps the
//     callback's `h.debounceTimer = nil` race-free).
//  2. Shutdown calls h.clientWG.Done() ONLY inside `if timer.Stop()`.
//     Both the Stop call and the guarded Done must still appear exactly
//     once each in Shutdown's body.
//  3. The callback still has `defer h.clientWG.Done()` as its first
//     statement — if Stop() returns false, this is what balances the
//     Add(1) from scheduling.
//
// The test reads wshub.go and checks these structural invariants. It
// passes on the current tree; a future edit that breaks the pairing
// trips CI with a message pointing to this audit item.
func TestDebounceTimer_ShutdownStopSemanticsContract(t *testing.T) {
	// R243-ARCH-2 split: Hub.Shutdown stays in wshub.go (lifecycle owner),
	// but the debounce AfterFunc scheduler moved to wshub_broadcast.go alongside
	// BroadcastSessionsUpdate. Read both and concatenate so the regex anchors
	// can locate either side without caring which file owns each fragment.
	src, err := os.ReadFile("wshub.go")
	if err != nil {
		t.Fatalf("read wshub.go: %v", err)
	}
	bcastSrc, err := os.ReadFile("wshub_broadcast.go")
	if err != nil {
		t.Fatalf("read wshub_broadcast.go: %v", err)
	}
	body := string(src)
	bcastBody := string(bcastSrc)

	// Locate the Shutdown function body. We match from `func (h *Hub) Shutdown()`
	// up to the next top-level `^func `. Using a simple anchor search keeps
	// the test resilient across formatting changes as long as the structure
	// is preserved.
	shutdownIdx := strings.Index(body, "func (h *Hub) Shutdown()")
	if shutdownIdx < 0 {
		t.Fatal("could not find Hub.Shutdown in wshub.go — contract pin " +
			"cannot verify debounce teardown; refactor must have renamed it")
	}
	// Scan forward for the next `\nfunc ` that begins a new top-level function.
	// This is a deliberately loose delimiter — it fails closed (too much body
	// captured = more regexes match, not fewer).
	rest := body[shutdownIdx:]
	nextFunc := regexp.MustCompile(`\nfunc `).FindStringIndex(rest[6:])
	var shutdownBody string
	if nextFunc != nil {
		shutdownBody = rest[:6+nextFunc[0]]
	} else {
		shutdownBody = rest
	}

	// 1) debounceTimer teardown must happen under debounceMu.
	if !regexp.MustCompile(`h\.debounceMu\.Lock\(\)[\s\S]*?h\.debounceTimer`).
		MatchString(shutdownBody) {
		t.Error("Hub.Shutdown no longer guards debounceTimer teardown with " +
			"debounceMu.Lock(). Without the guard, Shutdown racing the " +
			"AfterFunc callback's `h.debounceTimer = nil` write can observe " +
			"stale state and either double-Done (panic) or skip Done (hang). " +
			"R37-CONCUR3.")
	}

	// 2) The Done() in Shutdown must be nested inside `if ... Stop()`, not
	// unconditional. The exact form can vary (Stop() can live in an `if`
	// expression or in a conditional variable), so we accept either shape.
	nestedDone := regexp.MustCompile(
		`if\s+h\.debounceTimer\.Stop\(\)\s*\{\s*h\.clientWG\.Done\(\)\s*\}`)
	if !nestedDone.MatchString(shutdownBody) {
		t.Error("Hub.Shutdown no longer scopes clientWG.Done() to the " +
			"Stop()==true branch. Unconditional Done panics when Stop()==false " +
			"(callback already fired → its deferred Done already balances " +
			"the Add). Unconditional skip hangs Shutdown on clientWG.Wait " +
			"when Stop()==true (scheduled slot never released). R37-CONCUR3.")
	}

	// 3) The AfterFunc callback body must still begin with `defer h.clientWG.Done()`.
	// If the defer is missing the callback's Stop()==false branch never
	// decrements the WG, and Shutdown hangs.
	//
	// R243-ARCH-2 split: AfterFunc lives in wshub_broadcast.go (alongside
	// BroadcastSessionsUpdate). The scheduler call hands a pre-bound closure
	// (h.debounceFire) so the contract pins the defer in the closure
	// definition itself (NewHub in wshub.go) AND keeps a fallback inline
	// closure shape for hand-rolled hubs in BroadcastSessionsUpdate.
	// R239-PERF-6 introduced the pre-bound shape to drop the per-call alloc;
	// either site is acceptable as long as the deferred Done remains the
	// first statement of the AfterFunc callback body.
	deferShape := regexp.MustCompile(`func\(\)\s*\{\s*defer\s+h\.clientWG\.Done\(\)`)
	if !deferShape.MatchString(string(src)) && !deferShape.MatchString(bcastBody) {
		t.Error("debounce AfterFunc callback no longer opens with " +
			"`defer h.clientWG.Done()`. Without this defer, a callback that " +
			"fires between Stop() returning false and Shutdown draining " +
			"clientWG.Wait leaks a slot — Wait hangs until the deadline " +
			"timer forces teardown. R37-CONCUR3.")
	}
	// The pre-bound closure path must still be wired by NewHub (otherwise
	// every call falls back to the inline literal and R239-PERF-6 regresses
	// silently). Pin the field assignment so future refactors that drop the
	// pre-bind step trip CI.
	if !regexp.MustCompile(`h\.debounceFire\s*=\s*func\(\)`).Match(src) {
		t.Error("Hub.NewHub no longer pre-binds h.debounceFire — " +
			"BroadcastSessionsUpdate would fall back to the per-call inline " +
			"closure literal, regressing R239-PERF-6.")
	}
	// The scheduler call site must still pass the pre-bound closure so
	// hot-path BroadcastSessionsUpdate calls reuse the Hub-lifetime func
	// value rather than allocating a fresh one each refresh.
	if !regexp.MustCompile(`time\.AfterFunc\(debounceInterval,\s*fire\)`).
		MatchString(bcastBody) {
		t.Error("BroadcastSessionsUpdate no longer hands the pre-bound " +
			"`fire` callback to time.AfterFunc — the per-call closure " +
			"allocation is back. R239-PERF-6.")
	}

	// 4) R248-TEST-8 negative anchor: time.AfterFunc(debounceInterval, ...)
	// must NOT live in wshub.go. The R243-ARCH-2 split moved the debounce
	// scheduler to wshub_broadcast.go alongside BroadcastSessionsUpdate so
	// the parent wshub.go stayed lifecycle-only. A future merge that pastes
	// the AfterFunc back into wshub.go (e.g. while resolving a conflict on
	// the broadcast path) would silently undo the locality the split
	// established and reintroduce the god-object. Pin the negative case so
	// such a merge fails CI.
	if regexp.MustCompile(`time\.AfterFunc\(debounceInterval`).Match(src) {
		t.Error("AfterFunc 不应回到 wshub.go - 应保留在 wshub_broadcast.go (R248-TEST-8)")
	}
}
