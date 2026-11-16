package claudefs

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The Agent tool's and the workflow runs' transcripts are listed; meta
// files, other names, symlinks and a run directory with a bad name are not.
func TestSubagentTranscripts(t *testing.T) {
	t.Parallel()
	l := newWFLayout(t)
	sub := SubagentsDir(l.projectDir, wfTestSID)
	write := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plain := filepath.Join(sub, "agent-a038d27202815ed2e.jsonl")
	write(plain)
	write(filepath.Join(sub, "agent-a038d27202815ed2e.meta.json"))
	write(filepath.Join(sub, "agent-x.jsonl")) // id too short
	write(filepath.Join(sub, "notes.jsonl"))
	write(filepath.Join(sub, "workflows", "not-a-run", "agent-a2093755b9a9ce8c1.jsonl"))
	outside := filepath.Join(t.TempDir(), "agent-a2093755b9a9ce8c2.jsonl")
	write(outside)
	if err := os.Symlink(outside, filepath.Join(l.runDir, "agent-a2093755b9a9ce8c2.jsonl")); err != nil {
		t.Fatal(err)
	}
	got := SubagentTranscripts(l.projectDir, wfTestSID)
	slices.Sort(got)
	want := []string{plain, filepath.Join(l.runDir, "agent-"+wfTestAgent+".jsonl")}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("SubagentTranscripts = %v\nwant %v", got, want)
	}
	if got := SubagentTranscripts(l.projectDir, "../x"); got != nil {
		t.Fatalf("bad session ID listed %v", got)
	}
	if got := SubagentTranscripts(l.projectDir, "04a8fc10-6fa5-4b8e-82ba-000000000000"); got != nil {
		t.Fatalf("absent session listed %v", got)
	}
}

// A sub-agents directory that is a symlink is not followed.
func TestSubagentTranscripts_SymlinkedDir(t *testing.T) {
	t.Parallel()
	l := newWFLayout(t)
	other := filepath.Join(l.root, "-other")
	if err := os.MkdirAll(filepath.Join(other, "s", "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "s", "subagents", "agent-a2093755b9a9ce8c0.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sid := "04a8fc10-6fa5-4b8e-82ba-111111111111"
	if err := os.MkdirAll(filepath.Join(l.projectDir, sid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "s", "subagents"), SubagentsDir(l.projectDir, sid)); err != nil {
		t.Fatal(err)
	}
	if got := SubagentTranscripts(l.projectDir, sid); got != nil {
		t.Fatalf("followed a symlinked sub-agents dir: %v", got)
	}
}
