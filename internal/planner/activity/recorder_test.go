package activity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

// A turn's end is journaled as kind=turn under the session, in the
// workspace that holds its directory (none for a directory no workspace
// holds), with how it ended.
func TestRecordTurn(t *testing.T) {
	j, err := journal.Open(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	reg := workspace.Load("", "node")
	dir := filepath.Join(t.TempDir(), "w")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w, ok := reg.Ensure(dir)
	if !ok {
		t.Fatal("workspace not registered")
	}
	r := NewRecorder(j, reg)
	r.RecordTurn("k1", dir, TurnDone)
	r.RecordTurn("k2", "/nowhere", TurnFailed)
	r.RecordTurn("k1", dir, TurnAborted)

	es, err := j.Read(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	type rec struct {
		unit, ws string
		outcome  TurnOutcome
	}
	var got []rec
	for _, e := range es {
		if e.Kind != journal.KindTurn {
			t.Fatalf("entry of kind %q, want only turns", e.Kind)
		}
		var d Turn
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		got = append(got, rec{e.Unit, e.WS, d.Outcome})
	}
	want := []rec{{"k1", w.ID, TurnDone}, {"k2", "", TurnFailed}, {"k1", w.ID, TurnAborted}}
	if len(got) != len(want) {
		t.Fatalf("journaled %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
	if n := len(r.Outputs("", 10)); n != 0 {
		t.Errorf("outputs index holds %d entries, want none for turn ends", n)
	}

	// No journal: nothing to write, and no panic.
	NewRecorder(nil, reg).RecordTurn("k1", dir, TurnDone)
}
