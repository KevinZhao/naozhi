package upstream

import (
	"fmt"
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestDerivedCaps_AdvertisesEventEntrySchema pins that every register
// handshake carries the EventEntry schema tag (#2496), and that the tag
// string tracks the SchemaVersion constant.
func TestDerivedCaps_AdvertisesEventEntrySchema(t *testing.T) {
	t.Parallel()
	if want := fmt.Sprintf("evententry.v%d", clievent.SchemaVersion); clievent.SchemaCap != want {
		t.Fatalf("SchemaCap=%q, want %q — bump the tag with SchemaVersion", clievent.SchemaCap, want)
	}
	if caps := derivedCaps(); !slices.Contains(caps, clievent.SchemaCap) {
		t.Errorf("derivedCaps() = %v, must always include %q", caps, clievent.SchemaCap)
	}
}
