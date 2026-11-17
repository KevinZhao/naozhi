package subagent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestLinker_Resolve_LateFlushOfTaskIDTranscript: on a live start the CLI emits
// task_started before agent-<task_id>.jsonl exists, and name is the task
// description, not the meta's agentType. The transcript appearing during the
// grace window must still link, rather than the window ending in a tombstone
// that every later lookup returns for the life of the process.
func TestLinker_Resolve_LateFlushOfTaskIDTranscript(t *testing.T) {
	t.Parallel()
	const sessionID = "11111111-2222-3333-4444-555555555555"
	const taskID = "ac5af15e0dc475fd1"
	l, subagentDir := newLinkerForTest(t, sessionID)
	now := time.Now()

	// The transcript lands after Resolve's first stat has missed: the hook
	// runs at the first agentType scan inside the retry loop.
	var wrote atomic.Bool
	l.scanHook = func() {
		if wrote.CompareAndSwap(false, true) {
			writeAgentFiles(t, subagentDir, taskID, "general-purpose", sessionID, "p1", now)
		}
	}

	info, resolved := l.Resolve(context.Background(), taskID, "toolu_A",
		"Code/docs search on Orchid", "", now.Add(-50*time.Millisecond).UnixMilli())
	if !resolved {
		t.Fatalf("Resolve did not reach a verdict: %+v", info)
	}
	if !wrote.Load() {
		t.Fatal("scan hook never ran; the test did not exercise the late flush")
	}
	if info.InternalAgentID != "agent-"+taskID {
		t.Fatalf("InternalAgentID = %q, want %q (a tombstone hides the transcript)", info.InternalAgentID, "agent-"+taskID)
	}
	if got, _ := l.QueryOrResolveFast(taskID); got.InternalAgentID != "agent-"+taskID {
		t.Errorf("QueryOrResolveFast = %+v, want the linked transcript", got)
	}
}
