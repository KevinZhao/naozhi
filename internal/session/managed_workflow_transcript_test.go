package session

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestWorkflowBoard_AgentTranscriptPending is the Board concurrency row's
// drill-in item: an agent of a run whose directory is still resolving is
// pending, current and earlier attempt alike; an id no row has is none,
// and so is one that could not name a file.
func TestWorkflowBoard_AgentTranscriptPending(t *testing.T) {
	d := newFakeDisk()
	d.resolveGate = make(chan struct{})
	r := diskRig(t, d)
	p := &setProc{}
	w := runningWithRun("w1", wfRun)
	w.Agents[0].AgentID, w.Agents[0].PrevAgentIDs = "a00000000000000b2", []string{"a00000000000000b1"}
	odd := wfRow(2, workflow.AgentRunning)
	odd.AgentID = "a0/../../x"
	w.Agents = append(w.Agents, odd)
	p.publish(w)
	r.b.bind(p, "/ws")
	testhelper.Eventually(t, func() bool { return d.resolves.Load() == 1 }, 5*time.Second, "resolution never started")
	for _, id := range []string{"a00000000000000b2", "a00000000000000b1"} {
		if tr, st := r.b.AgentTranscript(id); st != TranscriptPending || tr.Open != nil || tr.Loc.TaskID != "w1" {
			t.Errorf("%s while resolving: status %d, %+v; want pending", id, st, tr)
		}
	}
	for _, id := range []string{"a00000000000000ff", "a0/../../x"} {
		if _, st := r.b.AgentTranscript(id); st != TranscriptNone {
			t.Errorf("%s: status %d, want none", id, st)
		}
	}
	close(d.resolveGate)
	r.settleIO(t)
}
