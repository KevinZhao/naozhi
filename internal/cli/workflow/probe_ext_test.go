package workflow_test

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/workflow"
)

func probeAgents(final workflow.AgentState) []workflow.Agent {
	row := func(idx, phase int, label, id string, started, queued, lastProgress, dur, tokens int64) workflow.Agent {
		return workflow.Agent{
			Index: idx, PhaseIndex: phase, Label: label, AgentID: id, Model: "claude-opus-5-5[1m]",
			State: workflow.AgentDone, Attempt: 1, QueuedAt: queued, StartedAt: started,
			LastProgressAt: lastProgress, DurationMS: dur, Tokens: tokens,
		}
	}
	rows := []workflow.Agent{
		row(1, 1, "A", "a2093755b9a9ce8c0", 1791170018847, 1791170018846, 1791170021030, 2182, 17739),
		row(2, 1, "B", "a0ba344a06862740f", 1791170018848, 1791170018846, 1791170026698, 5858, 17740),
		row(3, 2, "C", "aa4f131ad102bbc7f", 1791170026701, 1791170026700, 1791170031521, 4820, 17738),
	}
	rows[2].State = final
	return rows
}

var probePhases = []workflow.Phase{
	{Index: 1, Title: "Ask", Counts: workflow.Counts{Total: 2, Done: 2}},
	{Index: 2, Title: "Sum", Counts: workflow.Counts{Total: 1, Done: 1}},
}

// TestProbeLiveReplay feeds the whole capture live and checks every field
// of the resulting entry, then that it agrees with the run's result file.
func TestProbeLiveReplay(t *testing.T) {
	t.Parallel()
	lines := probeLines(t)
	tr := workflow.New(nil)
	t0 := time.UnixMilli(1791170018000)
	feed(t, tr, t0, lines...)
	got := only(t, tr)

	want := workflow.Workflow{
		TaskID: probeTask, RunID: "wf_2997921d-435", Name: "probe", Description: "tiny probe", Current: "Sum: C",
		Status: workflow.StatusCompleted, StartedAt: t0.Add(3 * time.Millisecond).UnixMilli(), EndedAt: 1791170031523,
		LastObservedAt: t0.Add(15 * time.Millisecond).UnixMilli(),
		Tokens:         53217, DurationMS: 12699,
		Counts: workflow.Counts{Total: 3, Done: 3}, Phases: probePhases, Agents: probeAgents(workflow.AgentDone),
		NotifySummary: `Dynamic workflow "tiny probe" completed`, Source: workflow.SourceStream,
		TrackerVersion: 10, SnapshotSeq: 5,
		LaunchTranscriptDir: "/home/u/.claude/projects/-private-tmp-nz-wfprobe/" + probeSession + "/subagents/workflows/wf_2997921d-435",
		SessionID:           probeSession,
		Src:                 workflow.FieldSrc{Name: workflow.NameFromLaunch, StartedAt: workflow.StartedFromLive, SessionID: workflow.SessionFromLaunch},
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("live probe:\n got %+v\nwant %+v", *got, want)
	}

	data, err := os.ReadFile(probeResult)
	if err != nil {
		t.Fatal(err)
	}
	rf, err := workflow.ParseResultFile(data)
	if err != nil {
		t.Fatal(err)
	}
	merged, ok := workflow.MergeResultFile(got, rf)
	if !ok {
		t.Fatal("result file with the probe's taskId not merged")
	}
	if merged.Status != got.Status || merged.Counts != got.Counts || merged.Tokens != got.Tokens ||
		!reflect.DeepEqual(merged.Agents, got.Agents) || !reflect.DeepEqual(merged.Phases, got.Phases) {
		t.Errorf("result file disagrees with the stream:\n file %+v\nstream %+v", *merged, *got)
	}
	if merged.StartedAt != 1791170018759 || merged.Src.StartedAt != workflow.StartedFromResultFile ||
		merged.Source != workflow.SourceResultFile || !merged.ResultLoaded {
		t.Errorf("merged header = started %d/%d source %q loaded %v", merged.StartedAt, merged.Src.StartedAt, merged.Source, merged.ResultLoaded)
	}
}

// TestProbeStepByStep checks the entry at the capture's turning points.
func TestProbeStepByStep(t *testing.T) {
	t.Parallel()
	lines := probeLines(t)
	tr := workflow.New(nil)
	t0 := time.UnixMilli(1791170018000)

	feed(t, tr, t0, lines[:6]...) // through the first snapshot
	w := only(t, tr)
	if w.Status != workflow.StatusRunning || w.Current != "Ask: A" || len(w.Agents) != 2 {
		t.Fatalf("after line 6: %+v", *w)
	}
	// Line 6's B has state "start" but neither agentId nor startedAt.
	if b := w.Agents[1]; b.State != workflow.AgentQueued || b.AgentID != "" {
		t.Errorf("line 6 agent B = %+v, want queued", b)
	}
	if w.Counts != (workflow.Counts{Total: 2, Queued: 1, Running: 1}) || w.Degraded != "" {
		t.Errorf("line 6 counts %+v degraded %q", w.Counts, w.Degraded)
	}

	feed(t, tr, t0, lines[6:11]...) // through the first Sum snapshot
	feed(t, tr, t0, lines[11])      // description / usage only
	w = only(t, tr)
	if len(w.Agents) != 3 || w.Agents[2].State != workflow.AgentRunning || w.Tokens != 53217 || w.SnapshotSeq != 4 {
		t.Fatalf("description-only line touched the rows or missed the usage: %+v", *w)
	}

	feed(t, tr, t0, lines[12:15]...) // final snapshot, background_tasks_changed, task_updated
	w = only(t, tr)
	if w.Status != workflow.StatusCompleted || w.EndedAt != 1791170031523 || w.NotifySummary != "" {
		t.Fatalf("task_updated did not end the run first: %+v", *w)
	}
}
