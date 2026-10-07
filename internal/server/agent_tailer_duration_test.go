package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/ring"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// textLine is one assistant text record of an agent transcript stamped at ts
// (RFC 3339; "" leaves the timestamp out).
func textLine(text, ts string) string {
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]},"sessionId":"s"`
	if ts != "" {
		line += `,"timestamp":"` + ts + `"`
	}
	return line + "}\n"
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "")); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// A tailer opened on an agent that has already finished replays its whole
// transcript in one poll; its duration is the transcript's span, not the
// milliseconds the tailer has been alive (#3646: a 45.9s agent showed 206ms).
func TestTailer_DurationOfFinishedAgentIsTranscriptSpan(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent-done.jsonl")
	appendLines(t, path,
		textLine("start", "2026-05-10T10:00:00Z"),
		textLine("middle", "2026-05-10T10:00:20Z"),
		textLine("answer", "2026-05-10T10:00:45.9Z"),
	)

	r := newTailerRegistry("")
	defer r.Shutdown()
	tl, ok := r.ensureTailer("k", "t1", "toolu", path, nil)
	if !ok {
		t.Fatal("ensureTailer failed")
	}
	tl.pollOnce()
	if got := tl.MetaSnapshot().DurationMS; got != 45900 {
		t.Fatalf("DurationMS = %d, want 45900 (first to last record)", got)
	}

	// The late subscriber's meta nudge carries the same figure.
	c, out := newCapturedClient(t, nil)
	if !r.attach(tailerKey{"k", "t1"}, c) {
		t.Fatal("attach failed")
	}
	deadline := time.After(time.Second)
	for {
		select {
		case msg := <-out:
			if msg.Type != "agent_meta" {
				continue
			}
			if msg.AgentMeta == nil || msg.AgentMeta.DurationMS != 45900 {
				t.Fatalf("agent_meta = %+v, want duration_ms 45900", msg.AgentMeta)
			}
			return
		case <-deadline:
			t.Fatal("no agent_meta after attach")
		}
	}
}

// A running agent's duration moves on with each record it writes.
func TestTailer_DurationOfRunningAgentGrows(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent-live.jsonl")
	appendLines(t, path, textLine("start", "2026-05-10T10:00:00Z"))

	r := newTailerRegistry("")
	defer r.Shutdown()
	tl, ok := r.ensureTailer("k", "t1", "toolu", path, nil)
	if !ok {
		t.Fatal("ensureTailer failed")
	}
	tl.pollOnce()
	if got := tl.MetaSnapshot().DurationMS; got != 0 {
		t.Fatalf("one record: DurationMS = %d, want 0", got)
	}
	appendLines(t, path, textLine("step", "2026-05-10T10:00:05Z"))
	tl.pollOnce()
	if got := tl.MetaSnapshot().DurationMS; got != 5000 {
		t.Fatalf("after 5s: DurationMS = %d, want 5000", got)
	}
	// A record without a timestamp neither moves nor resets it.
	appendLines(t, path, textLine("unstamped", ""))
	tl.pollOnce()
	if got := tl.MetaSnapshot().DurationMS; got != 5000 {
		t.Fatalf("unstamped record: DurationMS = %d, want 5000", got)
	}
	appendLines(t, path, textLine("more", "2026-05-10T10:00:12Z"))
	tl.pollOnce()
	if got := tl.MetaSnapshot().DurationMS; got != 12000 {
		t.Fatalf("after 12s: DurationMS = %d, want 12000", got)
	}
}

// A teammate's prompt arrives in a teammate-message wrapper that maps to no
// event; the run still starts there, so its first model turn counts.
func TestTailer_DurationStartsAtDroppedPrompt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent-mate.jsonl")
	appendLines(t, path,
		`{"type":"user","message":{"role":"user","content":"<teammate-message teammate_id=\"lister-1\">go</teammate-message>"},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`+"\n",
		textLine("first reply", "2026-05-10T10:00:08Z"),
		textLine("answer", "2026-05-10T10:00:10Z"),
	)
	r := newTailerRegistry("")
	defer r.Shutdown()
	tl, ok := r.ensureTailer("k", "t1", "toolu", path, nil)
	if !ok {
		t.Fatal("ensureTailer failed")
	}
	tl.pollOnce()
	if got := tl.MetaSnapshot().DurationMS; got != 10000 {
		t.Fatalf("DurationMS = %d, want 10000 (prompt to answer)", got)
	}
}

// A transcript replaced by another run reports the new run's span, not one
// stretched across both files.
func TestTailer_DurationFollowsRotation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent-rot.jsonl")
	appendLines(t, path, textLine("old", "2026-05-10T09:00:00Z"), textLine("old2", "2026-05-10T09:30:00Z"))
	r := newTailerRegistry("")
	defer r.Shutdown()
	tl, ok := r.ensureTailer("k", "t1", "toolu", path, nil)
	if !ok {
		t.Fatal("ensureTailer failed")
	}
	tl.pollOnce()
	if got := tl.MetaSnapshot().DurationMS; got != 1_800_000 {
		t.Fatalf("before rotation: DurationMS = %d, want 1800000", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	appendLines(t, path, textLine("new", "2026-05-10T10:00:00Z"), textLine("new2", "2026-05-10T10:00:03Z"))
	tl.pollOnce()
	if got := tl.MetaSnapshot().DurationMS; got != 3000 {
		t.Fatalf("after rotation: DurationMS = %d, want 3000", got)
	}
}

// A settled task keeps the CLI's own duration: a tailer opened on it later
// (a drill-in) must not replace it with a larger transcript span. A running
// one, or a settled one the CLI gave no duration, still takes the tailer's.
func TestTailerEnrich_SettledDurationStands(t *testing.T) {
	t.Parallel()
	r := newTailerRegistry("")
	for _, id := range []string{"done", "error", "nodur", "running", "spawned"} {
		r.byTask[tailerKey{"k", id}] = &agentTailer{meta: node.AgentMetaPatch{DurationMS: 900_000}}
	}
	snap := &session.SessionSnapshot{Key: "k", Subagents: []ring.SubagentInfo{
		{TaskID: "done", Status: "completed", DurationMS: 45_900},
		{TaskID: "error", Status: "error", DurationMS: 3_000},
		{TaskID: "nodur", Status: "completed"},
		{TaskID: "running", Status: "running", DurationMS: 100},
		{TaskID: "spawned", Status: "spawned"},
	}}
	r.enrich(snap)
	want := map[string]int64{"done": 45_900, "error": 3_000, "nodur": 900_000, "running": 900_000, "spawned": 900_000}
	for _, sa := range snap.Subagents {
		if sa.DurationMS != want[sa.TaskID] {
			t.Errorf("%s: DurationMS = %d, want %d", sa.TaskID, sa.DurationMS, want[sa.TaskID])
		}
	}
}
