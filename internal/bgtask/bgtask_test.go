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
			tool("Monitor", 0), start("m1", "local_bash", "watch CI", time.Second), tool("Bash", 2*time.Second), start("b1", "local_bash", "make", 3*time.Second)}},
			want: []want{{"m1", KindMonitor, time.Second}, {"b1", KindBash, 3 * time.Second}}, progress: 3 * time.Second},
		{name: "a workflow is the board's, not its task frames", in: Input{
			Entries:   []clievent.EventEntry{start("w1", clievent.TaskTypeWorkflow, "wf", 0)},
			Workflows: []Workflow{{ID: "w2", Name: "release", Start: since, Seen: since.Add(time.Hour)}}},
			want: []want{{"w2", KindWorkflow, time.Hour}}, progress: time.Hour},
		{name: "one start per task, in start order", in: Input{Entries: []clievent.EventEntry{
			start("b1", "local_bash", "a", 0), start("b2", "local_bash", "b", time.Second), start("b1", "local_bash", "a", 2*time.Second), done("b2", 3*time.Second)}},
			want: []want{{"b1", KindBash, 2 * time.Second}}, progress: 2 * time.Second},
		// A /new, a --resume or a crash respawned the process: what the
		// earlier one started died with it.
		{name: "before the process started", in: Input{Entries: []clievent.EventEntry{
			start("b0", "local_bash", "old", -time.Minute), start("b1", "local_bash", "new", time.Second)}},
			want: []want{{"b1", KindBash, time.Second}}, progress: time.Second},
		{name: "the process exited", in: Input{Alive: false, Entries: []clievent.EventEntry{
			start("b1", "local_bash", "sleep 3600", time.Second)}}},
		// Its end lost: the limit by kind, from its last start or progress.
		{name: "a Bash past its limit", in: Input{Now: since.Add(BashQuiet + 2*time.Second), Entries: []clievent.EventEntry{
			start("b1", "local_bash", "bench", time.Second)}}},
		{name: "a Bash within its limit", in: Input{Now: since.Add(BashQuiet), Entries: []clievent.EventEntry{
			start("b1", "local_bash", "bench", time.Second)}},
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
			start("b1", "local_bash", "bench", time.Second), {Type: clievent.KindSystem, Summary: "restart", Time: ms(time.Hour)}}},
			want: []want{{"b1", KindBash, time.Second}}, progress: time.Second},
		{name: "ended in the replay", in: Input{
			Entries:  []clievent.EventEntry{start("b1", "local_bash", "bench", time.Second), start("b2", "local_bash", "ci", 2*time.Second)},
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
	}})
	if len(got.Tasks) != 2 || got.Tasks[0].Summary != "go test ./..." || got.Tasks[1].Summary != "b2" || !got.Tasks[1].Start.Equal(time.UnixMilli(2000)) {
		t.Fatalf("tasks = %+v", got.Tasks)
	}
}
