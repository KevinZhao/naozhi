package wireup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/session"
)

// The cwds a transcript ran in, each once in the order first seen; lines
// without one, relative ones and garbage are skipped.
func TestTranscriptCwds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"type":"user","cwd":"/r"}
{"type":"assistant","cwd":"/r/.claude/worktrees/x"}
not json "cwd":
{"type":"summary"}
{"cwd":"rel"}
{"type":"user","cwd":"/r"}
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var nilCache *TranscriptDirs
	if got := nilCache.read(p, false); !slices.Equal(got, []string{"/r", "/r/.claude/worktrees/x"}) {
		t.Fatalf("read = %v", got)
	}
	if got := nilCache.read(filepath.Join(t.TempDir(), "none"), false); got != nil {
		t.Fatalf("missing transcript = %v", got)
	}
}

// bashLine is a transcript line of an assistant turn calling Bash with cmd.
func bashLine(cwd, cmd string) string {
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(cmd)
	return `{"type":"assistant","cwd":"` + cwd + `","message":{"role":"assistant","content":[{"type":"text","text":"x"},{"type":"tool_use","id":"t","name":"Bash","input":{"command":"` + q + `"}}]}}` + "\n"
}

// A sub-agent's transcript keeps the workspace as its cwd; the directories
// its commands cd into are what show where it worked. Only a sub-agent's
// commands are read so.
func TestTranscriptCommandDirs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.jsonl")
	body := bashLine("/r", "cd /w/naozhiwt/a && go test ./...") +
		bashLine("/r", "git -C /w/naozhiwt/b status") +
		`{"type":"assistant","cwd":"/r","message":{"content":[{"type":"tool_use","name":"Read","input":{"command":"cd /w/not-bash"}}]}}` + "\n" +
		`{"type":"user","cwd":"/r","message":{"content":"cd /w/plain-text"}}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewTranscriptDirs()
	if got, want := c.read(p, true), []string{"/r", "/w/naozhiwt/a", "/w/naozhiwt/b"}; !slices.Equal(got, want) {
		t.Fatalf("sub-agent dirs = %v, want %v", got, want)
	}
	if got := NewTranscriptDirs().read(p, false); !slices.Equal(got, []string{"/r"}) {
		t.Fatalf("session's own transcript read for commands: %v", got)
	}
}

func TestCommandDirs(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	for _, c := range []struct {
		cmd  string
		want []string
	}{
		{"cd /a/b && make", []string{"/a/b"}},
		{`cd "/a/with space" ; ls`, []string{"/a/with space"}},
		{"cd '/a/q'", []string{"/a/q"}},
		{"cd /a/x/; cd /a/y&&pwd", []string{"/a/x", "/a/y"}},
		{"(cd /a/sub && ls) | head", []string{"/a/sub"}},
		{"pushd /a/p >/dev/null", []string{"/a/p"}},
		{"git -C /a/g log", []string{"/a/g"}},
		{"cd ~/workspace/wt", []string{filepath.Join(home, "workspace/wt")}},
		{"cd $HOME/wt", []string{filepath.Join(home, "wt")}},
		{"cd relative && cd -", nil},
		{"cd $T/x", nil},
		{"abcd /a/no", nil},
		{"echo cd", nil},
	} {
		if got := commandDirs(c.cmd); !slices.Equal(got, c.want) {
			t.Errorf("commandDirs(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

// A transcript is read on from where the last read stopped; a line still
// being written is read once complete, and one rewritten shorter is read
// again from the start.
func TestTranscriptDirsIncremental(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(`{"cwd":"/a"}`+"\n"+`{"cwd":"/b`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewTranscriptDirs()
	if got := c.read(p, false); !slices.Equal(got, []string{"/a"}) {
		t.Fatalf("first read = %v", got)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`"}` + "\n" + `{"cwd":"/c"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := c.read(p, false); !slices.Equal(got, []string{"/a", "/b", "/c"}) {
		t.Fatalf("after append = %v", got)
	}
	if off := c.files[p].off; off == 0 {
		t.Fatal("offset not kept")
	}
	if err := os.WriteFile(p, []byte(`{"cwd":"/z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := c.read(p, false); !slices.Equal(got, []string{"/z"}) {
		t.Fatalf("after rewrite = %v", got)
	}
	// A line past the cap is skipped, not the end of the read.
	long := `{"cwd":"/long","x":"` + strings.Repeat("x", maxTranscriptLine) + `"}` + "\n"
	if err := os.WriteFile(p, []byte(long+`{"cwd":"/after"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := NewTranscriptDirs().read(p, false); !slices.Equal(got, []string{"/after"}) {
		t.Fatalf("long line = %v", got)
	}
}

// subagentLayout is a Claude directory with session sid's transcript in
// workspace ws: its own lines (cwd main) and a workflow agent's.
func subagentLayout(t *testing.T, ws, sid string, main, agent string) string {
	t.Helper()
	claudeDir := t.TempDir()
	pd := claudefs.ProjectDir(claudeDir, ws)
	run := claudefs.WorkflowRunDir(claudefs.SubagentsDir(pd, sid), "wf_704c71ad-5d7")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudefs.TranscriptIn(pd, sid), []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "agent-a5dfd2d3ef5d7a860.jsonl"), []byte(agent), 0o600); err != nil {
		t.Fatal(err)
	}
	return claudeDir
}

const (
	reapSID = "78d5ed79-32c3-4539-b7c8-c9c7f9d90e2b"
)

// Dirs reads the workflow agents' transcripts too: the worktrees they cd'd
// into are put away with the session (11-14 22:28, three worktrees under
// ~/workspace/naozhiwt/ not seen).
func TestReapDirsWorkflowAgents(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "naozhi")
	claudeDir := subagentLayout(t, ws, reapSID, `{"cwd":"`+ws+`"}`+"\n",
		bashLine(ws, "cd /w/naozhiwt/autoupdate && go test ./...")+bashLine(ws, "cd /w/naozhiwt/reapreset && git status"))
	src := ReapSources{ClaudeDir: claudeDir, Transcripts: NewTranscriptDirs()}
	got := src.Dirs(ws, []string{reapSID})
	if want := []string{ws, "/w/naozhiwt/autoupdate", "/w/naozhiwt/reapreset"}; !slices.Equal(got, want) {
		t.Fatalf("Dirs = %v, want %v", got, want)
	}
}

// A worktree a session still in the router went into is in use, whether its
// own transcript's cwd or a workflow agent's commands went there: the
// put-away check of another session leaves it alone (meeting-m1, checked 19
// times by two put-away tasks while task:4fb2 still worked in it).
func TestReapInUseByTranscript(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "naozhi")
	wt := ws + "/.claude/worktrees/meeting-m1"
	claudeDir := subagentLayout(t, ws, reapSID, `{"cwd":"`+ws+`"}`+"\n"+`{"cwd":"`+wt+`/internal"}`+"\n",
		bashLine(ws, "cd /w/naozhiwt/autoupdate && go test ./..."))
	r := session.NewRouter(session.RouterConfig{})
	t.Cleanup(r.Shutdown)
	if got := r.RegisterForResume("task:4fb200000000", reapSID, ws, "继续"); got != "task:4fb200000000" {
		t.Fatalf("RegisterForResume = %q", got)
	}
	src := ReapSources{Router: r, ClaudeDir: claudeDir, Transcripts: NewTranscriptDirs()}
	for _, dir := range []string{wt, "/w/naozhiwt/autoupdate"} {
		if !src.InUse(dir, "task:445900000000") {
			t.Errorf("%s not in use", dir)
		}
		if src.InUse(dir, "task:4fb200000000") {
			t.Errorf("%s in use by the key's own session", dir)
		}
	}
	if src.InUse(ws+"/.claude/worktrees/other", "task:445900000000") {
		t.Error("a worktree no session went into is in use")
	}
	// A put-away session is out of the router: its directories no longer
	// hold anyone else's.
	if _, ok := r.Archive("task:4fb200000000"); !ok {
		t.Fatal("not archived")
	}
	if src.InUse(wt, "task:445900000000") {
		t.Error("in use by a put-away session")
	}
}
