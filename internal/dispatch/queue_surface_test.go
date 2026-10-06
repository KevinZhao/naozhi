package dispatch

import (
	"reflect"
	"sort"
	"testing"
)

// G-c (#2897 T3004): SessionRouter's exported method set equals an explicit
// list of names, not merely a count, so a rename is as visible as an
// addition. Both directions fail: a name added or removed must come with an
// edit to this list in the same change. #3004 has the plan that shrinks it.
// The *turn.Orchestrator half of G-c is internal/turn/queue_surface_test.go.
var sessionRouterMethodNames = []string{
	// /model, /effort and /backend (#3452): the dashboard's tuning surface
	// reached from IM. VisitSessions is the existing snapshot read and the
	// backend catalogue comes via Capabilities, so Router gains no method.
	"InterruptSessionViaControl",
	"ResetChatAndSetWorkspace",
	"SetSessionBackend",
	"SetSessionTuning",
	"VisitSessions",
	"Workspace",
}

// turnsMethodNames is G-c's Turns half: the dispatcher reaches the
// orchestrator for exactly these three (#3004 C2).
var turnsMethodNames = []string{
	"Reset",
	"ShouldNotify",
	"Submit",
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

// TestSessionRouterSurface_Ratchet pins G-c's SessionRouter half.
func TestSessionRouterSurface_Ratchet(t *testing.T) {
	t.Parallel()
	got := exportedMethodNames(reflect.TypeFor[SessionRouter]())
	assertMethodSet(t, "SessionRouter", got, sessionRouterMethodNames)
}

// TestTurnsSurface_Ratchet pins G-c's Turns half.
func TestTurnsSurface_Ratchet(t *testing.T) {
	t.Parallel()
	got := exportedMethodNames(reflect.TypeFor[Turns]())
	assertMethodSet(t, "Turns", got, turnsMethodNames)
}
