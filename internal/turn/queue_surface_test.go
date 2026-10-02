package turn

import (
	"reflect"
	"sort"
	"testing"
)

// G-c (#2897 T3004): Queue's exported method set equals an explicit list of
// names, not merely a count, so a rename is as visible as an addition. Both
// directions fail: a name added or removed must come with an edit to this
// list in the same change. Every name here has a production caller outside
// turn; #3004-E narrows the set further. SessionRouter's half of G-c is
// internal/dispatch/queue_surface_test.go.
var queueMethodNames = []string{
	"Cleanup",
	"CollectDelay",
	"Discard",
	"DiscardAndReturn",
	"DoneOrDrain",
	"Enqueue",
	"Mode",
	"ShouldNotify",
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

// TestQueueSurface_Ratchet pins G-c's Queue half.
func TestQueueSurface_Ratchet(t *testing.T) {
	t.Parallel()
	got := exportedMethodNames(reflect.TypeFor[*Queue]())
	assertMethodSet(t, "*Queue", got, queueMethodNames)
}

// TestOrchestratorSurface_Ratchet pins *Orchestrator's exported method set
// at #3004's final one: entry points submit, reset, rate-limit notices and
// clean up, and reach the Queue through nothing else.
func TestOrchestratorSurface_Ratchet(t *testing.T) {
	t.Parallel()
	got := exportedMethodNames(reflect.TypeFor[*Orchestrator]())
	assertMethodSet(t, "*Orchestrator", got, []string{"Cleanup", "Reset", "ShouldNotify", "Submit"})
}
