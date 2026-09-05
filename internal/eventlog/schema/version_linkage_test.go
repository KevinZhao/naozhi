package schema

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// wireSchemaAllowlist pins which clievent.SchemaVersion each WireVersion
// carries (#2496): bumping either constant without recording the new pairing
// here fails — the persisted-log format and the EventEntry semantics may not
// drift apart silently.
var wireSchemaAllowlist = map[int]int{
	1: 1, // WireVersion 1 carries EventEntry schema v1
}

func TestWireVersionPairsWithEventEntrySchema(t *testing.T) {
	t.Parallel()
	want, ok := wireSchemaAllowlist[WireVersion]
	if !ok {
		t.Fatalf("WireVersion=%d has no wireSchemaAllowlist entry — record the EventEntry schema pairing deliberately", WireVersion)
	}
	if clievent.SchemaVersion != want {
		t.Errorf("clievent.SchemaVersion=%d but WireVersion=%d is allowlisted for schema %d — bump the pairing together",
			clievent.SchemaVersion, WireVersion, want)
	}
}
