package dispatch

import (
	"reflect"
	"sort"
	"testing"
)

// G-c (#2897 T3004 A1): *MessageQueue's and SessionRouter's exported method
// sets equal an explicit list of names, not merely a count, so a rename
// (TryAcquire → Acquire) is as visible as an addition. Both directions fail:
// a name added or removed must come with an edit to its list here, in the
// same change. 14 and 8 names are #3004's measured state; #3004 has the plan
// that shrinks both lists.
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
