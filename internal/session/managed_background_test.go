package session

import (
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/shim"
)

// A shim replay's task frames become entries without times: starts, ends
// and the task_updated patch that may end one; a progress frame (a
// workflow's carries its whole snapshot) and live stdout are not read.
func TestReplayTaskEntries(t *testing.T) {
	t.Parallel()
	const (
		started  = `{"type":"system","subtype":"task_started","task_id":"b1","tool_use_id":"toolu_1","description":"make bench","task_type":"local_bash"}`
		progress = `{"type":"system","subtype":"task_progress","task_id":"a1","description":"reading"}`
		updated  = `{"type":"system","subtype":"task_updated","task_id":"a1","patch":{"status":"killed"}}`
		done     = `{"type":"system","subtype":"task_notification","task_id":"b1","status":"completed","summary":"make bench done"}`
	)
	replays := []shim.ServerMsg{
		{Type: "replay", Line: started},
		{Type: "replay", Line: progress},
		{Type: "replay", Line: updated},
		{Type: "stdout", Line: done},
		{Type: "replay", Line: done},
		{Type: "replay", Line: "not json"},
	}
	var got []string
	for _, e := range replayTaskEntries(&cli.ClaudeProtocol{}, replays) {
		if e.Time != 0 {
			t.Errorf("%s has a time", e.Type)
		}
		got = append(got, e.Type+"/"+e.TaskID+"/"+e.TaskType+"/"+e.Status+"/"+e.Summary)
	}
	want := []string{
		clievent.KindTaskStart + "/b1/local_bash//make bench",
		clievent.KindTaskProgress + "/a1//killed/",
		clievent.KindTaskDone + "/b1//completed/",
	}
	if !slices.Equal(got, want) {
		t.Errorf("replayTaskEntries = %q, want %q", got, want)
	}
}

// The CLI's start is the shim's: a reattached process is the one that ran
// before the restart. Without the info, or once another process is
// attached, it is naozhi's attach time and no replay is known.
func TestProcessStartedAt(t *testing.T) {
	t.Parallel()
	attached := time.Now()
	p := NewTestProcess()
	p.StartedAtVal = attached
	s := &ManagedSession{}
	s.storeProcess(p)
	if !s.ProcessStartedAt().Equal(attached) || s.ReplayedTasks() != nil {
		t.Fatalf("spawned: started %v replayed %v", s.ProcessStartedAt(), s.ReplayedTasks())
	}
	cliStart := attached.Add(-3 * time.Hour)
	tasks := []clievent.EventEntry{{Type: clievent.KindTaskDone, TaskID: "b1"}}
	s.SetReattachedForTest(cliStart, tasks)
	if !s.ProcessStartedAt().Equal(cliStart) || len(s.ReplayedTasks()) != 1 {
		t.Fatalf("reattached: started %v replayed %v", s.ProcessStartedAt(), s.ReplayedTasks())
	}
	p2 := NewTestProcess()
	p2.StartedAtVal = attached.Add(time.Minute)
	s.storeProcess(p2)
	if !s.ProcessStartedAt().Equal(p2.StartedAtVal) || s.ReplayedTasks() != nil {
		t.Errorf("respawned: started %v replayed %v", s.ProcessStartedAt(), s.ReplayedTasks())
	}
	if got := shimStartedAt(shim.State{StartedAt: "2026-11-08T10:00:00Z"}); !got.Equal(time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("shimStartedAt = %v", got)
	}
	if !shimStartedAt(shim.State{}).IsZero() {
		t.Error("shimStartedAt of no time is not zero")
	}
}
