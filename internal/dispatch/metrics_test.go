package dispatch

import (
	"expvar"
	"strconv"
	"testing"
)

// TestDispatchMetrics_Registered pins that the dispatch expvar mirror
// counters are registered under their canonical names so /debug/vars
// surfaces them. R245-ARCH-36 (#892): the dispatch.go atomic counters
// (messageCount / replyErrorCount / sendFailCount) used to be visible
// only via /health snapshots; this test guards the expvar wiring so
// future renames don't silently break operator dashboards.
func TestDispatchMetrics_Registered(t *testing.T) {
	wantNames := []string{
		"naozhi_dispatch_message_total",
		"naozhi_dispatch_reply_error_total",
		"naozhi_dispatch_send_fail_total",
	}
	for _, name := range wantNames {
		v := expvar.Get(name)
		if v == nil {
			t.Errorf("expvar.Get(%q) = nil; expected counter to be registered at package init", name)
			continue
		}
		if _, ok := v.(*expvar.Int); !ok {
			t.Errorf("expvar.Get(%q) = %T, want *expvar.Int", name, v)
		}
	}
}

// TestDispatchMetrics_AddCallable pins that the package-level mirror
// vars accept Add(1) — i.e. they are not nil-initialised. A
// dispatcher-level regression that swapped the var to a typed-nil
// interface would surface here as a nil deref.
func TestDispatchMetrics_AddCallable(t *testing.T) {
	before := dispatchMessageTotal.Value()
	dispatchMessageTotal.Add(1)
	after := dispatchMessageTotal.Value()
	if after-before != 1 {
		t.Errorf("dispatchMessageTotal.Add(1) changed value by %d, want 1 (before=%s after=%s)",
			after-before, strconv.FormatInt(before, 10), strconv.FormatInt(after, 10))
	}
	// Reset for hermeticity: subtract the bump so other tests that
	// inspect the snapshot don't see this perturbation. expvar.Int
	// has no Set(int64); use Add with negation.
	dispatchMessageTotal.Add(-1)
}
