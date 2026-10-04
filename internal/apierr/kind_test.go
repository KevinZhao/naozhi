package apierr

import "testing"

// TestKinds_EveryKindIsWorded: each Kind up to the last declared one has a
// friendly text and a log category, so no classified error renders empty.
func TestKinds_EveryKindIsWorded(t *testing.T) {
	t.Parallel()
	if len(kinds) != int(KindNetwork)+1 {
		t.Fatalf("kinds has %d entries, want one per Kind (%d)", len(kinds), KindNetwork+1)
	}
	for k, info := range kinds {
		if info.friendly == "" || info.category == "" {
			t.Errorf("Kind %d: friendly %q, category %q; want both set", k, info.friendly, info.category)
		}
	}
}
