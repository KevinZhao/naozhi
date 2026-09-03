package history

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/textutil"
)

// TestNewDerivedEntry_CanonicalRecipe pins the shared derivation recipe
// every fallback source (codexjsonl, kirojsonl) now rides on: caps of
// (SummaryMaxRunes, DetailMaxRunes) and a UUID over the TRUNCATED pair via
// textutil.DeriveLegacyUUID. #2336 happened because per-source copies of
// this recipe drifted; this test is the parity check that replaces them.
func TestNewDerivedEntry_CanonicalRecipe(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("s", SummaryMaxRunes+10) + strings.Repeat("d", DetailMaxRunes)
	e := NewDerivedEntry(1750500921000, "text", long)

	wantSummary := textutil.TruncateRunes(long, SummaryMaxRunes)
	wantDetail := textutil.TruncateRunes(long, DetailMaxRunes)
	if e.Summary != wantSummary || e.Detail != wantDetail {
		t.Errorf("caps drifted: len(Summary)=%d len(Detail)=%d", len(e.Summary), len(e.Detail))
	}
	if e.Time != 1750500921000 || e.Type != "text" {
		t.Errorf("Time/Type not carried through: %+v", e)
	}
	if want := textutil.DeriveLegacyUUID(e.Time, e.Type, e.Summary, e.Detail); e.UUID != want {
		t.Errorf("UUID %q, want canonical DeriveLegacyUUID over the truncated pair %q", e.UUID, want)
	}
}

// TestNewDerivedEntry_DetailDisambiguates re-pins #2336 at the shared
// recipe: same timestamp + same full Summary prefix, differing only in the
// detail tail, must derive distinct UUIDs or merged's UUID-first dedup
// silently drops one.
func TestNewDerivedEntry_DetailDisambiguates(t *testing.T) {
	t.Parallel()
	prefix := strings.Repeat("a", SummaryMaxRunes)
	e1 := NewDerivedEntry(1750500921000, "text", prefix+"-first tail")
	e2 := NewDerivedEntry(1750500921000, "text", prefix+"-second tail")
	if e1.Summary != e2.Summary {
		t.Fatalf("fixture must share the truncated summary")
	}
	if e1.UUID == e2.UUID {
		t.Errorf("detail tail not folded into UUID: both derive %q", e1.UUID)
	}
}
