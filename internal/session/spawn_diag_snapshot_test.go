package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestSnapshot_SpawnDiagsExposed pins the /api/sessions surface of #2532: a
// live process's gate decisions ride the snapshot, and a session without any
// (or without a process) still serialises spawn_diags as an empty array so
// the dashboard never sees undefined.
func TestSnapshot_SpawnDiagsExposed(t *testing.T) {
	r := newTestRouter(4)

	withDiags := &fakeProcess{spawnDiags: []cli.SpawnDiag{{
		Layer: "argv-denylist", Key: "--effort", Action: "dropped", Reason: "test",
	}}}
	injectSession(r, "diag:sess", withDiags)
	injectSession(r, "plain:sess", &fakeProcess{})

	for _, snap := range r.ListSessions() {
		data, err := json.Marshal(snap)
		if err != nil {
			t.Fatalf("marshal snapshot %q: %v", snap.Key, err)
		}
		if !strings.Contains(string(data), `"spawn_diags":[`) {
			t.Errorf("snapshot %q JSON lacks a spawn_diags array: %s", snap.Key, data)
		}
		switch snap.Key {
		case "diag:sess":
			if len(snap.SpawnDiags) != 1 || snap.SpawnDiags[0].Key != "--effort" {
				t.Errorf("diag:sess SpawnDiags = %+v, want the --effort drop", snap.SpawnDiags)
			}
		case "plain:sess":
			if len(snap.SpawnDiags) != 0 || snap.SpawnDiags == nil {
				t.Errorf("plain:sess SpawnDiags = %#v, want non-nil empty slice", snap.SpawnDiags)
			}
		}
	}
}
