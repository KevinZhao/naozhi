package wshub

import (
	"reflect"
	"testing"
)

// hubFieldCount is the machine-checked field count of wshub.Hub.
//
// The Hub godoc claims "49 fields organized in 7 blocks" as the Phase-4
// server.Hub mirror (server-split-phase4-design v0.6.1 §五). That contract
// was previously enforced only by hand-maintained comments, which #1499
// (R164930-ARCH-1) flagged as drift-prone: any field added or removed during
// the Phase-4b migration would silently desync the comment from reality. This
// constant + TestHubFieldCount turn the comment into a machine guarantee — a
// reflect-based assertion that fails CI the moment the struct shape changes
// without the documented count being updated in lockstep.
const hubFieldCount = 49

// TestHubFieldCount fails when wshub.Hub gains or loses a field without the
// godoc "49 fields" contract and hubFieldCount being updated together. This is
// the part-(b) guard from #1499: it does not delete the migration scaffold,
// it only pins the field count the field-block godoc advertises.
func TestHubFieldCount(t *testing.T) {
	got := reflect.TypeOf(Hub{}).NumField()
	if got != hubFieldCount {
		t.Fatalf("wshub.Hub has %d fields, want %d.\n"+
			"The Hub godoc advertises a fixed %d-field mirror of server.Hub "+
			"(server-split-phase4-design v0.6.1 §五). If you intentionally "+
			"added/removed a field, update both the godoc field-block comment "+
			"in hub.go AND hubFieldCount here so the two stay in sync (#1499).",
			got, hubFieldCount, hubFieldCount)
	}
}
