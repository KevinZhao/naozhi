package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDropMarshalCache_DecisionTable pins the dropMarshalCache predicate in
// handleUnsubscribe (wshub_subscribe.go ~346):
//
//	dropMarshalCache = !h.enforceCaps || h.subscriberCount[key] == 0
//
// PR #1421 review (R040034-CHANGES) flagged that the migration from the
// old `subscriberCount == nil || count == 0` shape to the new explicit
// `!enforceCaps || count == 0` shape changed the semantics for hand-rolled
// `&Hub{subscriberCount: make(...)}` fixtures (enforceCaps=false). The old
// code consulted the count map; the new code unconditionally drops on the
// not-enforced branch. The drop-on-unwired-fixtures behaviour is now the
// load-bearing contract — it stops a lingering historyMarshalCache slot
// from outliving a fixture's intended lifetime, and it short-circuits the
// counter read on hubs whose subscriberCount is shape-only (capacity but
// no semantic guarantee). Without a test, a future refactor that flips
// the gate back to `subscriberCount == nil || ...` would silently
// regress fixtures that test handleUnsubscribe by populating the map.
//
// We exercise the predicate as a pure-data table so the test is unaffected
// by the surrounding handleUnsubscribe goroutines / channels / lock
// hierarchy. The accompanying source pin guarantees the production code
// keeps consulting the same expression.
func TestDropMarshalCache_DecisionTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		enforceCaps bool
		count       int // h.subscriberCount[key]; 0 == "not present"
		wantDrop    bool
	}{
		{"!enforceCaps + count=0 → drop (no other subs)", false, 0, true},
		{"!enforceCaps + count=3 → drop (fixture-mode unconditional drop, R040034-CHANGES)", false, 3, true},
		{"enforceCaps + count=0 → drop (last subscriber gone, production hot path)", true, 0, true},
		{"enforceCaps + count>0 → keep (other subs still need cache)", true, 5, false},
		{"enforceCaps + count=1 → keep (one other sub)", true, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dropMarshalCacheDecision(tc.enforceCaps, tc.count)
			if got != tc.wantDrop {
				t.Errorf("dropMarshalCacheDecision(enforceCaps=%v, count=%d) = %v, want %v",
					tc.enforceCaps, tc.count, got, tc.wantDrop)
			}
		})
	}
}

// dropMarshalCacheDecision mirrors the predicate at wshub_subscribe.go:346
// inline so the test does not depend on running the full handleUnsubscribe
// state machine. It MUST stay byte-equal to the production expression —
// the source-pin test below catches any drift.
func dropMarshalCacheDecision(enforceCaps bool, count int) bool {
	return !enforceCaps || count == 0
}

// TestDropMarshalCache_SourcePin guards against the predicate at
// wshub_subscribe.go:346 silently changing shape. R040034-CHANGES
// documented the semantic difference between the old
// `subscriberCount == nil || ...` and the new `!enforceCaps || ...`;
// re-introducing the nil-guard would suppress the drop on enforceCaps=true
// + counter unwired, and re-introducing the count-only short-circuit
// without enforceCaps would skip drops on hand-rolled fixtures.
func TestDropMarshalCache_SourcePin(t *testing.T) {
	t.Parallel()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(self)
	src, err := os.ReadFile(filepath.Join(dir, "wshub_subscribe.go"))
	if err != nil {
		t.Fatalf("read wshub_subscribe.go: %v", err)
	}
	body := string(src)

	// Anchor the exact production expression so the in-test mirror
	// (dropMarshalCacheDecision) cannot drift away from production.
	const want = "dropMarshalCache = !h.enforceCaps || h.subscriberCount[key] == 0"
	if !strings.Contains(body, want) {
		t.Errorf("wshub_subscribe.go no longer contains the dropMarshalCache predicate %q — "+
			"if you intentionally changed the shape, update dropMarshalCacheDecision in this "+
			"test file in lockstep so the decision-table assertions still pin the live behaviour.",
			want)
	}

	// And explicitly forbid the pre-fix nil-guard shape: re-introducing it
	// would silently regress fixtures that allocate subscriberCount
	// directly without flipping enforceCaps (the contract change R040034
	// was explicitly designed to expose at the use-site, not hide behind
	// a defensive nil-guard).
	const forbidden = "dropMarshalCache = h.subscriberCount == nil || h.subscriberCount[key] == 0"
	if strings.Contains(body, forbidden) {
		t.Errorf("wshub_subscribe.go re-introduced the pre-fix nil-guard predicate %q — "+
			"R040034-CHANGES requires gating on h.enforceCaps so test fixtures that "+
			"allocate subscriberCount without flipping the bool keep the unconditional drop.",
			forbidden)
	}
}
