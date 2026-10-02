package dispatch

import (
	"reflect"
	"sort"
	"testing"
)

// G-c (#2897 T3004 A1): *MessageQueue's and SessionRouter's exported method
// sets equal an explicit list of names, not merely a count — renaming
// TryAcquire to Acquire would leave messageQueueMethodBaseline's count
// unchanged but is exactly the kind of surface drift this item exists to
// make visible before #3004's structural moves start. Both directions fail:
// a name added or removed, in either list, must come with an edit to the
// list it belongs to, here, in the same change.
//
// 14 names is #3004's measured state (6 of them — Depth, TryAcquire,
// ShouldSendWait, Release, ReleaseWithDrain, SetStrandHandler — have no
// production caller; B deletes those six). 8 names is dispatch.SessionRouter's
// current interface (7 of them have a production caller; SetWorkspace does
// not). B shrinks MessageQueue's list to the 8 names that survive; C2 shrinks
// SessionRouter's list to 3 (Workspace, ResetChatAndSetWorkspace,
// InterruptSessionViaControl).
var messageQueueMethodNames = []string{
	"Cleanup",
	"CollectDelay",
	"Depth",
	"Discard",
	"DiscardAndReturn",
	"DoneOrDrain",
	"Enqueue",
	"Mode",
	"Release",
	"ReleaseWithDrain",
	"SetStrandHandler",
	"ShouldNotify",
	"ShouldSendWait",
	"TryAcquire",
}

var sessionRouterMethodNames = []string{
	"DiscardPassthroughPending",
	"GetOrCreate",
	"InterruptSessionViaControl",
	"NotifyIdle",
	"Reset",
	"ResetChatAndSetWorkspace",
	"SetWorkspace",
	"Workspace",
}

func exportedMethodNames(t reflect.Type) []string {
	names := make([]string, 0, t.NumMethod())
	for i := range t.NumMethod() {
		names = append(names, t.Method(i).Name)
	}
	sort.Strings(names)
	return names
}

func assertMethodSet(t *testing.T, label string, got []string, want []string) {
	t.Helper()
	wantSorted := append([]string{}, want...)
	sort.Strings(wantSorted)
	if reflect.DeepEqual(got, wantSorted) {
		return
	}
	gotSet := map[string]bool{}
	for _, n := range got {
		gotSet[n] = true
	}
	wantSet := map[string]bool{}
	for _, n := range wantSorted {
		wantSet[n] = true
	}
	var extra, missing []string
	for _, n := range got {
		if !wantSet[n] {
			extra = append(extra, n)
		}
	}
	for _, n := range wantSorted {
		if !gotSet[n] {
			missing = append(missing, n)
		}
	}
	t.Errorf("%s exported method set changed: got %v, want %v (extra: %v, missing: %v) — new surface needs a reason, a shrunk one needs the list in this test lowered in the same change (#2897 T3004 G-c)",
		label, got, wantSorted, extra, missing)
}

// TestMessageQueueSurface_Ratchet pins G-c's first half.
func TestMessageQueueSurface_Ratchet(t *testing.T) {
	t.Parallel()
	got := exportedMethodNames(reflect.TypeFor[*MessageQueue]())
	assertMethodSet(t, "*MessageQueue", got, messageQueueMethodNames)
}

// TestSessionRouterSurface_Ratchet pins G-c's second half.
func TestSessionRouterSurface_Ratchet(t *testing.T) {
	t.Parallel()
	got := exportedMethodNames(reflect.TypeFor[SessionRouter]())
	assertMethodSet(t, "SessionRouter", got, sessionRouterMethodNames)
}
