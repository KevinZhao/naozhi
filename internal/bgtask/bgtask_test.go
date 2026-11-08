package bgtask

import (
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

func TestJudge(t *testing.T) {
	since := time.UnixMilli(1_000_000)
	ms := func(d time.Duration) int64 { return since.Add(d).UnixMilli() }
	start := func(id, typ, summary string, at time.Duration) clievent.EventEntry {
		return clievent.EventEntry{Type: clievent.KindTaskStart, TaskID: id, TaskType: typ, Summary: summary, Time: ms(at)}
	}
	progress := func(id, status string, at time.Duration) clievent.EventEntry {
		return clievent.EventEntry{Type: clievent.KindTaskProgress, TaskID: id, Status: status, Time: ms(at)}
	}
	done := func(id string, at time.Duration) clievent.EventEntry {
		return clievent.EventEntry{Type: clievent.KindTaskDone, TaskID: id, Status: "completed", Time: ms(at)}
	}
	tool := func(name string, at time.Duration) clievent.EventEntry {
		return clievent.EventEntry{Type: clievent.KindToolUse, Tool: name, Time: ms(at)}
	}
	result := clievent.EventEntry{Type: clievent.KindResult, Time: ms(time.Minute)}
	type want struct {
		id   string
		kind Kind
		last time.Duration
	}
	tests := []struct {
		name     string
		in       Input
		want     []want
		progress time.Duration
	}{
		{name: "none", in: Input{Entries: []clievent.EventEntry{result}}},
		{name: "a background Bash after its turn ended", in: Input{Entries: []clievent.EventEntry{
			tool("Bash", 0), start("b1", "local_bash", "npm run bench", time.Second), result}},
			want: []want{{"b1", KindBash, time.Second}}, progress: time.Second},
		{name: "start and done pair", in: Input{Entries: []clievent.EventEntry{
			start("b1", "local_bash", "sleep 1", 0), done("b1", time.Second), result}}},
		{name: "a patch with an end status ends it", in: Input{Entries: []clievent.EventEntry{
			start("a1", "local_agent", "review", 0), progress("a1", "killed", time.Second)}}},
		{name: "a running patch is progress", in: Input{Entries: []clievent.EventEntry{
			start("a1", "local_agent", "review", 0), progress("a1", "running", 5*time.Minute)}},
			want: []want{{"a1", KindAgent, 5 * time.Minute}}, progress: 5 * time.Minute},
		{name: "a Monitor is the tool call before it", in: Input{Entries: []clievent.EventEntry{
			tool("Monitor", 0), start("m1", "local_bash", "watch CI", time.Second), tool("Bash", 2*time.Second), start("b1", "local_bash", "make", 3*time.Second), result}},
			want: []want{{"m1", KindMonitor, time.Second}, {"b1", KindBash, 3 * time.Second}}, progress: 3 * time.Second},
		{name: "a workflow is the board's, not its task frames", in: Input{
			Entries:   []clievent.EventEntry{start("w1", clievent.TaskTypeWorkflow, "wf", 0)},
			Workflows: []Workflow{{ID: "w2", Name: "release", Start: since, Seen: since.Add(time.Hour)}}},
			want: []want{{"w2", KindWorkflow, time.Hour}}, progress: time.Hour},
		{name: "one start per task, in start order", in: Input{Entries: []clievent.EventEntry{
			start("b1", "local_bash", "a", 0), start("b2", "local_bash", "b", time.Second), start("b1", "local_bash", "a", 2*time.Second), done("b2", 3*time.Second), result}},
			want: []want{{"b1", KindBash, 2 * time.Second}}, progress: 2 * time.Second},
		// A /new, a --resume or a crash respawned the process: what the
		// earlier one started died with it.
		{name: "before the process started", in: Input{Entries: []clievent.EventEntry{
			start("b0", "local_bash", "old", -time.Minute), start("b1", "local_bash", "new", time.Second), result}},
			want: []want{{"b1", KindBash, time.Second}}, progress: time.Second},
		{name: "the process exited", in: Input{Alive: false, Entries: []clievent.EventEntry{
			start("b1", "local_bash", "sleep 3600", time.Second)}}},
		// Its end lost: the limit by kind, from its last start or progress.
		{name: "a Bash past its limit", in: Input{Now: since.Add(BashQuiet + 2*time.Second), Entries: []clievent.EventEntry{
			start("b1", "local_bash", "bench", time.Second)}}},
		{name: "a Bash within its limit", in: Input{Now: since.Add(BashQuiet), Entries: []clievent.EventEntry{
			start("b1", "local_bash", "bench", time.Second), result}},
			want: []want{{"b1", KindBash, time.Second}}, progress: time.Second},
		{name: "an agent quiet past its limit", in: Input{Now: since.Add(AgentQuiet + time.Hour), Entries: []clievent.EventEntry{
			start("a1", "local_agent", "review", 0), progress("a1", "", time.Hour)}},
			want: []want{{"a1", KindAgent, time.Hour}}, progress: time.Hour},
		{name: "an agent quiet past it", in: Input{Now: since.Add(AgentQuiet + time.Hour + time.Second), Entries: []clievent.EventEntry{
			start("a1", "local_agent", "review", 0), progress("a1", "", time.Hour)}}},
		{name: "a workflow has no limit", in: Input{Now: since.Add(48 * time.Hour),
			Workflows: []Workflow{{ID: "w1", Start: since}}},
			want: []want{{"w1", KindWorkflow, 0}}},
		// After a restart: the event log has the start, the shim's replay
		// the end the gateway missed while it was down.
		{name: "restored from the event log", in: Input{Entries: []clievent.EventEntry{
			start("b1", "local_bash", "bench", time.Second), result, {Type: clievent.KindSystem, Summary: "restart", Time: ms(time.Hour)}}},
			want: []want{{"b1", KindBash, time.Second}}, progress: time.Second},
		{name: "ended in the replay", in: Input{
			Entries:  []clievent.EventEntry{start("b1", "local_bash", "bench", time.Second), start("b2", "local_bash", "ci", 2*time.Second), result},
			Replayed: []clievent.EventEntry{{Type: clievent.KindTaskDone, TaskID: "b1"}}},
			want: []want{{"b2", KindBash, 2 * time.Second}}, progress: 2 * time.Second},
		{name: "started in the replay only", in: Input{
			Replayed: []clievent.EventEntry{{Type: clievent.KindTaskStart, TaskID: "b3", TaskType: "local_bash", Summary: "deploy"}}},
			want: []want{{"b3", KindBash, 0}}},
		// The replay re-sends frames the log already has: an end before a
		// repeated start in it does not bring the task back.
		{name: "an end anywhere ends it", in: Input{
			Entries:  []clievent.EventEntry{start("b1", "local_bash", "bench", time.Second), done("b1", 2*time.Second)},
			Replayed: []clievent.EventEntry{{Type: clievent.KindTaskStart, TaskID: "b1", TaskType: "local_bash"}}}},
		// A task only the replay started was open at the attach: its
		// quiet counts from there, not from the shim's start.
		{name: "started in the replay only, attached later", in: Input{Attached: since.Add(time.Hour),
			Replayed: []clievent.EventEntry{{Type: clievent.KindTaskStart, TaskID: "b3", TaskType: "local_bash"}}},
			want: []want{{"b3", KindBash, time.Hour}}, progress: time.Hour},
		// A foreground Bash the turn waits on writes task frames too: it
		// is the turn running until the model writes again.
		{name: "a foreground Bash", in: Input{Entries: []clievent.EventEntry{
			tool("Bash", 0), start("b1", "local_bash", "go test ./...", time.Second)}}},
		{name: "a foreground Bash that ended", in: Input{Entries: []clievent.EventEntry{
			tool("Bash", 0), start("b1", "local_bash", "go test ./...", time.Second), done("b1", 2*time.Second),
			{Type: clievent.KindText, Time: ms(3 * time.Second)}}}},
		{name: "a Bash the turn moved past", in: Input{Entries: []clievent.EventEntry{
			tool("Bash", 0), start("b1", "local_bash", "npm run dev", time.Second), tool("Read", 2*time.Second)}},
			want: []want{{"b1", KindBash, time.Second}}, progress: time.Second},
		{name: "an agent is never a foreground Bash", in: Input{Entries: []clievent.EventEntry{
			start("a1", "local_agent", "review", 0)}},
			want: []want{{"a1", KindAgent, 0}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			if in.Now.IsZero() {
				in.Now = since.Add(2 * time.Hour)
			}
			if tt.name != "the process exited" {
				in.Alive = true
			}
			in.Since = since
			got := Judge(in)
			var ids []want
			for _, task := range got.Tasks {
				ids = append(ids, want{task.ID, task.Kind, task.Last.Sub(since)})
			}
			if !slices.Equal(ids, tt.want) {
				t.Fatalf("tasks = %+v, want %+v", ids, tt.want)
			}
			if got.Count() != len(tt.want) || got.Open() != (len(tt.want) > 0) {
				t.Errorf("Count %d Open %v for %d tasks", got.Count(), got.Open(), len(tt.want))
			}
			if len(tt.want) > 0 && !got.LastProgress.Equal(since.Add(tt.progress)) {
				t.Errorf("LastProgress = %v, want %v", got.LastProgress.Sub(since), tt.progress)
			}
		})
	}
}

// A task's summary is the CLI's description, its ID when it has none.
func TestJudgeSummary(t *testing.T) {
	now := time.UnixMilli(5000)
	got := Judge(Input{Alive: true, Now: now, Entries: []clievent.EventEntry{
		{Type: clievent.KindTaskStart, TaskID: "b1", TaskType: "local_bash", Summary: "go test ./...", Time: 1000},
		{Type: clievent.KindTaskStart, TaskID: "b2", TaskType: "local_bash", Time: 2000},
		{Type: clievent.KindResult, Time: 3000},
	}})
	if len(got.Tasks) != 2 || got.Tasks[0].Summary != "go test ./..." || got.Tasks[1].Summary != "b2" || !got.Tasks[1].Start.Equal(time.UnixMilli(2000)) {
		t.Fatalf("tasks = %+v", got.Tasks)
	}
}

// The scans of one process hand on what their windows lose: a task whose
// start left the window stays open until its end is seen, and a task the
// replay started and the event log ended stays ended once its end has
// left the window too.
func TestScanFromCarry(t *testing.T) {
	since := time.UnixMilli(1_000_000)
	at := func(d time.Duration) int64 { return since.Add(d).UnixMilli() }
	filler := func(n int, from time.Duration) []clievent.EventEntry {
		var out []clievent.EventEntry
		for i := range n {
			out = append(out, clievent.EventEntry{Type: clievent.KindToolUse, Tool: "Read", Time: at(from) + int64(i)})
		}
		return out
	}
	ids := func(s State) []string {
		var out []string
		for _, t := range s.Tasks {
			out = append(out, t.ID)
		}
		return out
	}
	in := func(entries ...clievent.EventEntry) Input {
		return Input{Alive: true, Since: since, Attached: since.Add(time.Minute), Entries: entries,
			Replayed: []clievent.EventEntry{{Type: clievent.KindTaskStart, TaskID: "r1", TaskType: "local_bash", Summary: "deploy"}}}
	}

	// Scan 1: the replay's r1 open, b1 started in the window.
	s, c := ScanFrom(Carry{}, in(append([]clievent.EventEntry{
		{Type: clievent.KindToolUse, Tool: "Bash", Time: at(time.Minute)},
		{Type: clievent.KindTaskStart, TaskID: "b1", TaskType: "local_bash", Summary: "bench", Time: at(2 * time.Minute)}},
		filler(3, 3*time.Minute)...)...))
	if got := ids(s); !slices.Equal(got, []string{"b1", "r1"}) {
		t.Fatalf("scan 1 = %v, want b1 r1", got)
	}
	// Scan 2: r1's end in the log; b1's start has left the window.
	s, c = ScanFrom(c, in(append(filler(3, 4*time.Minute),
		clievent.EventEntry{Type: clievent.KindTaskDone, TaskID: "r1", Status: "completed", Time: at(5 * time.Minute)})...))
	if got := ids(s); !slices.Equal(got, []string{"b1"}) || !s.Tasks[0].Last.Equal(since.Add(2*time.Minute)) {
		t.Fatalf("scan 2 = %+v, want b1 carried with its times", s.Tasks)
	}
	// Scan 3: both have left the window; the replay still starts r1.
	s, c = ScanFrom(c, in(filler(3, 6*time.Minute)...))
	if got := ids(s); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("scan 3 = %v, want b1 only: r1 ended", got)
	}
	// Scan 4: b1's end.
	s, c = ScanFrom(c, in(clievent.EventEntry{Type: clievent.KindTaskDone, TaskID: "b1", Status: "completed", Time: at(7 * time.Minute)}))
	if s.Open() {
		t.Fatalf("scan 4 = %v, want none", ids(s))
	}
	if s, _ = ScanFrom(c, in(filler(3, 8*time.Minute)...)); s.Open() {
		t.Fatalf("scan 5 = %v, want none", ids(s))
	}
	// Without the carry the replay brings r1 back: the bug the carry fixes.
	if s := Scan(in(filler(3, 8*time.Minute)...)); !slices.Equal(ids(s), []string{"r1"}) {
		t.Fatalf("first scan = %v, want r1 from the replay", ids(s))
	}
}

// A foreground Bash carried from a window that ended on its start counts
// once the model writes after it, not for an output before its start in
// the next window.
func TestScanFromCarryForeground(t *testing.T) {
	since := time.UnixMilli(1_000_000)
	text := func(ms int64) clievent.EventEntry { return clievent.EventEntry{Type: clievent.KindText, Time: ms} }
	start := clievent.EventEntry{Type: clievent.KindTaskStart, TaskID: "b1", TaskType: "local_bash", Summary: "make", Time: 1_002_000}
	s, c := ScanFrom(Carry{}, Input{Alive: true, Since: since, Entries: []clievent.EventEntry{text(1_001_000), start}})
	if s.Open() {
		t.Fatalf("a Bash the turn waits on counted: %+v", s.Tasks)
	}
	s, c = ScanFrom(c, Input{Alive: true, Since: since, Entries: []clievent.EventEntry{text(1_001_000), start}})
	if s.Open() {
		t.Fatalf("the output before its start counted it: %+v", s.Tasks)
	}
	if s, _ = ScanFrom(c, Input{Alive: true, Since: since, Entries: []clievent.EventEntry{text(1_003_000)}}); s.Count() != 1 {
		t.Fatalf("the turn moved past it and it is not counted: %+v", s.Tasks)
	}
}

// Expires is when the first task with a limit drops; a workflow has none.
func TestExpires(t *testing.T) {
	t0 := time.UnixMilli(1_000_000)
	s := State{Tasks: []Task{
		{ID: "w1", Kind: KindWorkflow, Last: t0},
		{ID: "b1", Kind: KindBash, Last: t0},
		{ID: "a1", Kind: KindAgent, Last: t0.Add(time.Hour)},
	}}
	at, ok := s.Expires()
	if want := t0.Add(time.Hour + AgentQuiet); !ok || !at.After(want) || at.Sub(want) > time.Millisecond {
		t.Fatalf("Expires = %v %v, want just after %v", at, ok, want)
	}
	if s.At(at).Count() != 2 || s.At(at.Add(-time.Nanosecond)).Count() != 3 {
		t.Errorf("At around Expires: %d then %d", s.At(at.Add(-time.Nanosecond)).Count(), s.At(at).Count())
	}
	if _, ok := (State{Tasks: []Task{{ID: "w1", Kind: KindWorkflow}}}).Expires(); ok {
		t.Error("a workflow expires")
	}
	if _, ok := (State{}).Expires(); ok {
		t.Error("none open expires")
	}
}
