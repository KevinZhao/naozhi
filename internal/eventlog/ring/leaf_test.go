package ring

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// allowedInternalImports is the complete set of naozhi packages the ring may
// depend on. Everything else it needs is stdlib.
var allowedInternalImports = map[string]bool{
	"github.com/naozhi/naozhi/internal/cli/clievent": true,
	"github.com/naozhi/naozhi/internal/textutil":     true,
}

// TestRingIsALeaf asserts the ring's whole internal-import surface, which is the
// property R35-REL2 was really about: the event log must never reach into Hub
// state, because a subMu-holding subscriber callback that acquires h.mu is an
// ABBA deadlock against Shutdown.
//
// That invariant used to be policed from internal/server by grepping
// ../cli/eventlog*.go for the string "internal/server" (see
// shutdown_lock_order_test.go part B). Two things changed when the ring became
// its own package (#2545 G1): the scan's path went stale — it failed loudly
// rather than passing vacuously, which is why it was written that way — and
// `ring` importing `server` became a genuine compile-time cycle
// (server → cli → ring), verified by probe.
//
// The compiler guarantee is contingent, though: it only holds while something in
// that chain still imports the ring. So the check moved here and got stronger.
// Instead of one forbidden import it pins the ALLOWED set, which catches
// internal/server, internal/session, internal/dispatch and anything else that
// would turn a leaf into a participant. Adding an entry to
// allowedInternalImports is the deliberate act of widening it.
func TestRingIsALeaf(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var prod []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			prod = append(prod, f)
		}
	}
	// A vacuous pass would be worse than a failure: the whole point is that the
	// set is small, so an empty scan trivially satisfies it.
	if len(prod) < 5 {
		t.Fatalf("found %d non-test files (%v); the scan looks wrong and this "+
			"assertion would pass for the wrong reason", len(prod), prod)
	}

	importRe := regexp.MustCompile(`"(github\.com/naozhi/naozhi/[^"]+)"`)
	found := map[string]string{} // import path -> first file that has it
	for _, f := range prod {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range importRe.FindAllStringSubmatch(string(src), -1) {
			if _, ok := found[m[1]]; !ok {
				found[m[1]] = f
			}
		}
	}
	var offenders []string
	for path, file := range found {
		if !allowedInternalImports[path] {
			offenders = append(offenders, path+" (in "+file+")")
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("the event-log ring must stay a leaf; unexpected naozhi imports:\n  %s\n"+
			"R35-REL2: the ring must never reach into Hub state — a subMu-holding "+
			"subscriber callback that acquires h.mu deadlocks Shutdown. If a new "+
			"dependency is genuinely needed, add it to allowedInternalImports in "+
			"the same commit and say why.", strings.Join(offenders, "\n  "))
	}
}
