package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

var testPR = clievent.CodeChange{Provider: "github", URL: "https://github.com/o/r/pull/12", Repo: "o/r", Identifier: "12", Action: "created", Branch: "feat-x"}

// codeChangeProc is a process whose only behaviour is the code-change hook.
type codeChangeProc struct {
	processIface
	fn func(clievent.CodeChange)
}

func (p *codeChangeProc) SetOnCodeChange(fn func(clievent.CodeChange)) { p.fn = fn }

// A reported PR lands on the session, fires the change hook and shows in the
// snapshot; the CLI's repeat announcement of it does neither again.
func TestBookCodeChanges_RecordsPersistsAndDedupes(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 1})
	t.Cleanup(func() { r.Shutdown() })
	s := newSessionWithID("dash:direct:pr:general", "sess-pr-1")
	proc := &codeChangeProc{}
	bookCodeChanges(s, proc, func() { r.ss.Update(markChanged) })
	if proc.fn == nil {
		t.Fatal("bookCodeChanges bound no hook")
	}

	r.ss.Update(func(tx sessTx) { tx.SetDirty(false) })
	proc.fn(testPR)
	var dirty bool
	r.ss.View(func(v sessView) { dirty = v.Dirty() })
	if !dirty {
		t.Error("a new PR did not mark the table for saving")
	}
	if got := s.snapshot(false).CodeChanges; len(got) != 1 || got[0] != testPR {
		t.Fatalf("snapshot code_changes = %+v, want [%+v]", got, testPR)
	}

	r.ss.Update(func(tx sessTx) { tx.SetDirty(false) })
	proc.fn(testPR)
	r.ss.View(func(v sessView) { dirty = v.Dirty() })
	if dirty || len(s.CodeChanges()) != 1 {
		t.Errorf("repeat announcement: dirty=%v list=%+v, want a no-op", dirty, s.CodeChanges())
	}
}

// The list survives a restart, and a hand-edited entry whose URL is not an
// http(s) link is dropped at load while the rest of the session restores.
func TestCodeChangesPersist_RoundTripAndLoadFilter(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	key := "dash:direct:pr:general"
	src := newSessionWithID(key, "sess-pr-1")
	src.recordCodeChange(testPR)
	if err := saveStore(storePath, map[string]*ManagedSession{key: src}); err != nil {
		t.Fatalf("saveStore: %v", err)
	}

	var entries []map[string]any
	raw, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) != 1 {
		t.Fatalf("store = %s (err %v)", raw, err)
	}
	ccs, _ := entries[0]["code_changes"].([]any)
	if len(ccs) != 1 {
		t.Fatalf("persisted code_changes = %v, want one entry", entries[0]["code_changes"])
	}
	entries[0]["code_changes"] = append(ccs, map[string]any{"url": "javascript:alert(1)", "identifier": "13"})
	if raw, err = json.Marshal(entries); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewRouter(RouterConfig{MaxProcs: 1, StorePath: storePath})
	t.Cleanup(func() { r.Shutdown() })
	var got *ManagedSession
	r.ss.View(func(v sessView) { got = v.Get(key) })
	if got == nil {
		t.Fatal("session not restored")
	}
	if list := got.CodeChanges(); len(list) != 1 || list[0] != testPR {
		t.Errorf("restored code_changes = %+v, want only [%+v]", list, testPR)
	}
}
