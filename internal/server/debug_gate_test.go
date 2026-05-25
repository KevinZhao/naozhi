package server

import (
	"os"
	"regexp"
	"testing"
)

// TestPprofExpvarGatedByDebugMode pins R244-SEC-P3-1 [REPEAT-3]: the pprof
// + expvar route registrations must sit inside an `if s.debugMode` block in
// dashboard.go's setupRoutes. Without the gate, an authenticated dashboard
// caller from loopback (every operator session in a UDS / SSH-tunnel deploy)
// could enumerate goroutine stacks (which embed file paths and queue
// contents) and expvar counters at all times — turning a leaked dashboard
// token into a host-fingerprint primitive.
//
// The fix is already in place (`if s.debugMode { s.registerPprof();
// s.registerExpvar() }`); this test pins it so a refactor that splits the
// two registrations or removes the gate trips a compile-time signal.
//
// Reading dashboard.go source mirrors TestDashboardWiring_RegistersPprof
// in cmd/naozhi/main_helpers_test.go — narrow source contract that updates
// when the wiring genuinely moves.
func TestPprofExpvarGatedByDebugMode(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("dashboard.go")
	if err != nil {
		t.Fatalf("read dashboard.go: %v", err)
	}
	src := string(data)

	// Match `if s.debugMode {` followed by both registerPprof() and
	// registerExpvar() within the same block (closed by `}`). Tolerant
	// of whitespace and comments between the registrations.
	gate := regexp.MustCompile(`(?s)if\s+s\.debugMode\s*\{[^}]*s\.registerPprof\(\)[^}]*s\.registerExpvar\(\)[^}]*\}`)
	if !gate.MatchString(src) {
		t.Fatal(`dashboard.go must register pprof + expvar inside ` +
			`"if s.debugMode { s.registerPprof(); s.registerExpvar() }". ` +
			`A debug_mode flag is the R244-SEC-P3-1 [REPEAT-3] mitigation: ` +
			`without the gate, an authenticated loopback caller can dump ` +
			`goroutine stacks and expvar counters whenever the server is ` +
			`up. See docs/ops/pprof.md for the runbook.`)
	}

	// Defense-in-depth: ensure no UN-gated registerPprof() / registerExpvar()
	// call sneaks in elsewhere in the file. Find every call site and check
	// each is preceded by the gate keyword within the same logical block.
	for _, fn := range []string{"s.registerPprof()", "s.registerExpvar()"} {
		// Scan all occurrences. The gated block matched above counts as one
		// occurrence per fn; any additional bare call is an ungated one.
		count := 0
		for i := 0; ; {
			j := i + len(src[i:])
			idx := indexOf(src[i:j], fn)
			if idx < 0 {
				break
			}
			count++
			i = i + idx + len(fn)
		}
		if count != 1 {
			t.Errorf("%s appears %d times in dashboard.go; want exactly 1 (gated). "+
				"A second un-gated call would defeat the debug_mode guard.", fn, count)
		}
	}
}

// indexOf is strings.Index inlined to keep this test file's import surface
// minimal (it already imports os/regexp/testing for the source scan).
func indexOf(haystack, needle string) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
