package server

import (
	"os"
	"regexp"
	"testing"
)

// TestHubShutdown_LockOrderInvariant is the R35-REL2 pin for the
// h.mu → eventLog.subMu lock ordering documented on Hub.Shutdown.
// Shutdown invokes per-key unsub closures while holding h.mu; each
// closure ends up taking eventLog.l.subMu (write lock) via
// EventLog.Unsubscribe. The inverse direction — any code path that
// acquires subMu first and then tries to take h.mu — creates an
// ABBA deadlock that surfaces only at Shutdown time, long after
// the offending change merged.
//
// Today the invariant holds because:
//
//  1. notifySubscribers holds subMu.RLock and touches no Hub state.
//  2. eventPushLoop reads the Hub's context (h.ctx) via a value
//     captured when the goroutine was spawned, never calls h.mu.Lock.
//  3. readPump / writePump use hub.unregister which DOES take h.mu,
//     but they are invoked from the goroutine's own stack — not
//     from inside an EventLog callback.
//
// Guard all three properties at source level:
//
//	A. wshub.go (this package) must not contain any lexical pattern
//	   where a function acquires subMu and then h.mu.
//	B. eventlog.go (internal/cli) must not import internal/server —
//	   if it did, a direct h.mu access from a subMu-holding callback
//	   would become possible without this test catching it.
//	C. The LOCK ORDER CONTRACT comment stays in Shutdown's godoc so
//	   future readers follow the chain back to this audit item.
//
// Any failure here forces the author to re-evaluate whether the
// new lock site can starve Shutdown.
func TestHubShutdown_LockOrderInvariant(t *testing.T) {
	wshubSrc, err := os.ReadFile("wshub.go")
	if err != nil {
		t.Fatalf("read wshub.go: %v", err)
	}

	// C) the LOCK ORDER CONTRACT tripwire comment must survive.
	if !regexp.MustCompile(`LOCK ORDER CONTRACT \(R35-REL2\)`).Match(wshubSrc) {
		t.Error("Hub.Shutdown no longer carries the LOCK ORDER CONTRACT godoc. " +
			"R35-REL2: the comment is the only anchor linking the h.mu → " +
			"eventLog.subMu ordering to this audit item. If you reorganise " +
			"the comment, keep the R35-REL2 tag searchable so the contract " +
			"test can still locate it.")
	}

	// A) within wshub.go, reject any function body that acquires
	// subMu (hypothetical future code accessing EventLog directly)
	// AND also has a subsequent h.mu.Lock / h.mu.RLock.
	//
	// We use a conservative heuristic: within ~1000 chars of any
	// `subMu.Lock(` or `subMu.RLock(` call, no `h.mu.Lock(` /
	// `h.mu.RLock(` should appear. This catches the obvious
	// reversed-order patterns; more creative violations (passing
	// the hub into a subMu-holding callback) are out of scope for
	// a lexical test but would need a runtime -race reproducer
	// instead.
	wshubStr := string(wshubSrc)
	subMuRe := regexp.MustCompile(`subMu\.(?:R?Lock)\(`)
	for _, m := range subMuRe.FindAllStringIndex(wshubStr, -1) {
		end := m[1] + 1000
		if end > len(wshubStr) {
			end = len(wshubStr)
		}
		window := wshubStr[m[1]:end]
		if regexp.MustCompile(`h\.mu\.(?:R?Lock)\(`).MatchString(window) {
			t.Errorf("wshub.go acquires subMu at offset %d and then h.mu within "+
				"the next ~1000 chars. R35-REL2: h.mu must be acquired BEFORE "+
				"subMu (the Shutdown path) — the inverse ordering creates an "+
				"ABBA deadlock.", m[0])
		}
	}

	// B) eventlog.go in internal/cli must not import internal/server.
	// A circular-ish import would also fail to compile, but a thin
	// bridge via an interface would slip through compile and still
	// break the invariant. We check the import list explicitly.
	eventlogPath := "../cli/eventlog.go"
	if _, statErr := os.Stat(eventlogPath); statErr == nil {
		eventlogSrc, err := os.ReadFile(eventlogPath)
		if err == nil {
			if regexp.MustCompile(`"github\.com/naozhi/naozhi/internal/server"`).Match(eventlogSrc) {
				t.Error("internal/cli/eventlog.go imports internal/server. R35-REL2: " +
					"EventLog must never reach into Hub state, or a subMu-holding " +
					"callback could trigger h.mu acquisition and deadlock Shutdown.")
			}
		}
	}
}
