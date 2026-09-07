package server

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// wshubLockOrderScanFiles returns every non-test .go file in this package whose
// name starts with "wshub" — the files that hold Hub state and could plausibly
// touch EventLog subscription primitives in the same translation unit.
//
// This was a hand-maintained []string until #2560. The list's own comment
// admitted the failure mode it created: "forces a deliberate choice when a new
// wshub_ file is added: either include it here or leave a paper trail". In
// practice a new file that nobody remembered to register was simply not scanned,
// so invariant A went quietly green over it — the R248-TEST-4 incident the list
// was introduced to fix (PR #327 split wshub.go and the lock-acquisition code
// migrated into files the scan did not read) is the same failure the list itself
// could reproduce.
//
// Auto-discovery removes the registration step. A new wshub_*.go is covered the
// moment it exists, and deleting one cannot leave a stale entry that fails the
// read.
func wshubLockOrderScanFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, "wshub") || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	// The Hub is spread across several wshub_*.go files; a scan that found one
	// or none means the glob (or the package layout) changed and invariant A is
	// no longer looking at the Hub.
	if len(out) < 5 {
		t.Fatalf("only %d wshub*.go files discovered (%v) — invariant A would scan almost nothing", len(out), out)
	}
	return out
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
//	   logic — see wshubLockOrderScanFiles.)
//	B. the eventlog*.go files (internal/cli) must not import
//	   internal/server — if any did, a direct h.mu access from a
//	   subMu-holding callback would become possible without this test
//	   catching it. ARCH-EVENTLOG-SPLIT spread EventLog across siblings,
//	   so the scan globs every eventlog*.go (mirrors part A / PR #327).
//
// Any failure here forces the author to re-evaluate whether the
// new lock site can starve Shutdown.
func TestHubShutdown_LockOrderInvariant(t *testing.T) {
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
	// Any single-letter-ish receiver, not just `h`: #2551 introduced
	// (e *sendEngine) methods, and a hard-coded `h.mu.` would make invariant A
	// blind to every receiver named anything else — a silently-passing test.
	hMuRe := regexp.MustCompile(`\b[a-z][a-zA-Z0-9]*\.mu\.(?:R?Lock)\(`)
	for _, file := range wshubLockOrderScanFiles(t) {
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
