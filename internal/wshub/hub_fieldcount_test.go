// hub_fieldcount_test.go — machine guard for the Hub struct's documented
// field-block contract.
//
// R164930-ARCH-1 (#1499) concern (b): the Phase-4a skeleton Hub's
// "49 fields organized in 7 blocks" contract (hub.go godoc) is otherwise
// maintained only by a hand-counted comment. With no machine assertion the
// count drifts silently as fields are added during the Phase-4b sync to
// server.Hub. This test pins the total via reflection so any field
// add/remove forces a deliberate update of BOTH the godoc block accounting
// AND wantHubFields below — turning "keep the comment in sync by hand" into
// a CI failure rather than a latent drift.
//
// This is intentionally a count-only guard: it does NOT couple wshub to
// internal/server (which would re-introduce the import the skeleton exists
// to avoid) and makes no claim about the migration's end state. When
// Phase 4b adds fields, bump wantHubFields and the godoc block tally
// together in the same change.
package wshub

import (
	"reflect"
	"testing"
)

// wantHubFields is the field count the hub.go godoc accounts for across its
// 7 blocks: lifecycle 3 + subscriber 12 + broadcast 6 + send 6 +
// shared 14 + tailer 3 + cache 5 = 49.
const wantHubFields = 49

func TestHubFieldCountMatchesContract(t *testing.T) {
	t.Parallel()
	got := reflect.TypeOf(Hub{}).NumField()
	if got != wantHubFields {
		t.Fatalf("wshub.Hub has %d fields, contract (hub.go godoc) declares %d.\n"+
			"A field was added/removed without updating the field-block tally.\n"+
			"Update BOTH the hub.go '7 blocks = N' godoc accounting AND wantHubFields "+
			"in the same change (R164930-ARCH-1/#1499).", got, wantHubFields)
	}
}
