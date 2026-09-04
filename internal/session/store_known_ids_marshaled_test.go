package session

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/naozhi/naozhi/internal/session/knownids"
)

// TestKnownIDsMarshalSnapshot_MatchesSaveKnownIDs verifies the memoised
// MarshalSnapshot bytes handed to saveKnownIDsBytes are byte-identical to the
// saveKnownIDs marshal of SortedSnapshot, so both save paths write the same
// deterministic file.
func TestKnownIDsMarshalSnapshot_MatchesSaveKnownIDs(t *testing.T) {
	var kid knownids.Store
	for _, id := range []string{"zeta", "alpha", "mike", "delta"} {
		kid.Track(id)
	}

	memoised, err := kid.MarshalSnapshot()
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	legacy, err := json.Marshal(kid.SortedSnapshot())
	if err != nil {
		t.Fatalf("legacy marshal: %v", err)
	}
	if string(memoised) != string(legacy) {
		t.Errorf("memoised marshal differs from legacy:\n memoised %q\n legacy   %q", memoised, legacy)
	}

	tmp := t.TempDir()
	p1 := tmp + "/a/sessions.json"
	p2 := tmp + "/b/sessions.json"
	if err := saveKnownIDsBytes(p1, memoised); err != nil {
		t.Fatalf("saveKnownIDsBytes: %v", err)
	}
	if err := saveKnownIDs(p2, kid.SortedSnapshot()); err != nil {
		t.Fatalf("saveKnownIDs: %v", err)
	}
	b1, err := os.ReadFile(knownIDsPath(p1))
	if err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(knownIDsPath(p2))
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Errorf("on-disk bytes differ between save paths:\n %q\n %q", b1, b2)
	}
}
