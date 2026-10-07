package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
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

// The span is earliest to latest whatever order the records arrive in.
func TestTailer_DurationSpanIgnoresOrder(t *testing.T) {
	t.Parallel()
	tl := &agentTailer{}
	for _, at := range []int64{20_000, 45_900, 0, 1_000} {
		tl.updateMetaFromEventLocked(clievent.EventEntry{Type: clievent.KindText, Time: at})
	}
	if got := tl.meta.DurationMS; got != 44_900 {
		t.Fatalf("DurationMS = %d, want 44900", got)
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
