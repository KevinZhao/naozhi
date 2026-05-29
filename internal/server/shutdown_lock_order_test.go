package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// wshubLockOrderFiles enumerates every file in this package that is
// allowed to mention Hub state and EventLog subscription primitives
// in the same translation unit. Keeping this list explicit (rather
// than globbing wshub_*.go) forces a deliberate choice when a new
// wshub_ file is added: either include it here so invariant A guards
// it, or leave a paper trail explaining why it is exempt.
//
// R248-TEST-4: PR #327 split the original wshub.go god-object into
// six files and the lock-acquisition code that invariant A is meant
// to police migrated to wshub_subscribe.go / wshub_eventpush.go.
// Reading only wshub.go made the scan empty (a silent green test).
var wshubLockOrderFiles = []string{
	"wshub.go",
	"wshub_broadcast.go",
	"wshub_send.go",
	"wshub_subscribe.go",
	"wshub_eventpush.go",
	"wshub_upgrade.go",
	"wshub_agent.go",
}

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
//	A. None of the wshub_*.go files in this package may contain a
//	   lexical pattern where a function acquires subMu and then
//	   h.mu. (Originally only wshub.go was scanned; PR #327 split
//	   the file and the relevant lock sites moved to siblings, so
//	   the scan now covers every wshub_*.go that owns Hub or push
//	   logic — see wshubLockOrderFiles.)
//	B. the eventlog*.go files (internal/cli) must not import
//	   internal/server — if any did, a direct h.mu access from a
//	   subMu-holding callback would become possible without this test
//	   catching it. ARCH-EVENTLOG-SPLIT spread EventLog across siblings,
//	   so the scan globs every eventlog*.go (mirrors part A / PR #327).
//	C. The LOCK ORDER CONTRACT comment stays in Shutdown's godoc so
//	   future readers follow the chain back to this audit item.
//
// Any failure here forces the author to re-evaluate whether the
// new lock site can starve Shutdown.
func TestHubShutdown_LockOrderInvariant(t *testing.T) {
	// C) the LOCK ORDER CONTRACT tripwire comment must survive in
	// wshub.go (Hub.Shutdown's godoc anchor). Read it eagerly so we
	// fail fast if the file vanished.
	wshubSrc, err := os.ReadFile("wshub.go")
	if err != nil {
		t.Fatalf("read wshub.go: %v", err)
	}
	if !regexp.MustCompile(`LOCK ORDER CONTRACT \(R35-REL2\)`).Match(wshubSrc) {
		t.Error("Hub.Shutdown no longer carries the LOCK ORDER CONTRACT godoc. " +
			"R35-REL2: the comment is the only anchor linking the h.mu → " +
			"eventLog.subMu ordering to this audit item. If you reorganise " +
			"the comment, keep the R35-REL2 tag searchable so the contract " +
			"test can still locate it.")
	}

	// A) within every wshub_*.go file in the package, reject any
	// function body that acquires subMu (hypothetical future code
	// accessing EventLog directly) AND also has a subsequent
	// h.mu.Lock / h.mu.RLock.
	//
	// We use a conservative heuristic: within ~1000 chars of any
	// `subMu.Lock(` or `subMu.RLock(` call, no `h.mu.Lock(` /
	// `h.mu.RLock(` should appear. This catches the obvious
	// reversed-order patterns; more creative violations (passing
	// the hub into a subMu-holding callback) are out of scope for
	// a lexical test but would need a runtime -race reproducer
	// instead.
	subMuRe := regexp.MustCompile(`subMu\.(?:R?Lock)\(`)
	hMuRe := regexp.MustCompile(`h\.mu\.(?:R?Lock)\(`)
	for _, file := range wshubLockOrderFiles {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		body := string(src)
		for _, m := range subMuRe.FindAllStringIndex(body, -1) {
			end := m[1] + 1000
			if end > len(body) {
				end = len(body)
			}
			window := body[m[1]:end]
			if hMuRe.MatchString(window) {
				t.Errorf("%s acquires subMu at offset %d and then h.mu within "+
					"the next ~1000 chars. R35-REL2: h.mu must be acquired BEFORE "+
					"subMu (the Shutdown path) — the inverse ordering creates an "+
					"ABBA deadlock.", file, m[0])
			}
		}
	}

	// B) the eventlog*.go files in internal/cli must not import
	// internal/server. A circular-ish import would also fail to compile,
	// but a thin bridge via an interface would slip through compile and
	// still break the invariant. We check the import list explicitly.
	//
	// ARCH-EVENTLOG-SPLIT moved PersistSink + the rest of EventLog out of
	// the single eventlog.go into sibling files (eventlog_persist.go,
	// eventlog_subscribe.go, …); like part A's PR #327 widening, the scan
	// now globs every eventlog*.go so the no-import invariant still holds
	// if a subMu-holding callback is added in any of them.
	serverImportRe := regexp.MustCompile(`"github\.com/naozhi/naozhi/internal/server"`)
	eventlogFiles, globErr := filepath.Glob("../cli/eventlog*.go")
	if globErr != nil {
		t.Fatalf("glob ../cli/eventlog*.go: %v", globErr)
	}
	if len(eventlogFiles) == 0 {
		t.Fatal("no ../cli/eventlog*.go files found; R35-REL2 import guard would " +
			"silently pass — verify the path before trusting this test")
	}
	for _, path := range eventlogFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if serverImportRe.Match(src) {
			t.Errorf("%s imports internal/server. R35-REL2: "+
				"EventLog must never reach into Hub state, or a subMu-holding "+
				"callback could trigger h.mu acquisition and deadlock Shutdown.", path)
		}
	}
}
