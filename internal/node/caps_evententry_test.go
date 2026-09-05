package node

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestKnownServerCaps_IncludeEventEntrySchema pins that the primary
// recognises the EventEntry schema tag remote nodes advertise (#2496); a
// SchemaVersion bump that forgets this side would WARN-spam every register.
// The expected tag is spelled out as a literal on purpose: asserting
// knownServerCaps[clievent.SchemaCap] would be a tautology (the map key IS
// that constant) and could never catch a version bump.
func TestKnownServerCaps_IncludeEventEntrySchema(t *testing.T) {
	t.Parallel()
	const wantTag = "evententry.v1" // bump together with clievent.SchemaVersion
	if clievent.SchemaCap != wantTag {
		t.Fatalf("clievent.SchemaCap = %q — SchemaVersion bumped; update this literal AND knownServerCaps deliberately", clievent.SchemaCap)
	}
	if _, ok := knownServerCaps[wantTag]; !ok {
		t.Errorf("knownServerCaps lacks %q", wantTag)
	}
}
