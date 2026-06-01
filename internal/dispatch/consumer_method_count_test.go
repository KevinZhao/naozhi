package dispatch

import (
	"reflect"
	"testing"
)

// TestConsumerInterface_MethodCounts pins R246-ARCH-11 (#791): four packages
// (cron / dispatch / server / upstream) each declare their own SessionRouter
// consumer interface, and the documented decision (consumer.go header /
// docs/rfc/consumer-interfaces.md §3.4) is to keep dispatch's as a SINGLE
// small interface rather than splitting into Lookup/Lifecycle/Mutator — but
// only "as long as it stays small". The fragmentation risk #791 flags is
// silent growth: each new d.router.* call extends this interface, and once it
// crosses the small-interface threshold the no-split rationale stops holding
// and the split-before-more-drift work becomes overdue.
//
// This guard makes growth impossible to land unnoticed: bumping the count
// forces the author to update this test, which surfaces the decision point in
// review ("does adding method N mean we should finally do the Lookup/Lifecycle/
// Mutator split per #791?"). It is the cheap counterpart to the compile-time
// `var _ SessionRouter = (*session.Router)(nil)` pin, which catches signature
// drift but not method-set growth.
func TestConsumerInterface_MethodCounts(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		want int
	}{
		// 8 distinct d.router.* methods; see consumer.go. Crossing this is the
		// trigger to revisit the Lookup/Lifecycle/Mutator split (#791).
		{"SessionRouter", reflect.TypeOf((*SessionRouter)(nil)).Elem(), 8},
		// 5 distinct d.projectMgr.* methods; see consumer.go (ARCH-DISP-1 #457).
		{"ProjectStore", reflect.TypeOf((*ProjectStore)(nil)).Elem(), 5},
	}
	for _, c := range cases {
		if got := c.typ.NumMethod(); got != c.want {
			t.Errorf("dispatch.%s has %d methods, want %d. If you intentionally "+
				"added/removed a consumer method, update this count — and if "+
				"%s has grown past a 'small' interface, revisit the "+
				"Lookup/Lifecycle/Mutator split tracked in #791 before bolting "+
				"on more.", c.name, got, c.want, c.name)
		}
	}
}
