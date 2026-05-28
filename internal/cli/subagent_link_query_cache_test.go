package cli

import (
	"context"
	"testing"
	"time"
)

// TestLinker_Query_R241PERF5_CacheFastPath pins the contract relied on by
// process_readloop.notifyLinker / process_event_query.kick after R241-PERF-5
// (#478): once Resolve has terminally cached an entry, Query returns it
// O(1) so the dispatcher can skip the goroutine spawn for repeated
// task_started events with the same task_id.
//
// The bug this guards against is a future Query refactor that changes
// the return semantics for already-resolved entries (e.g. only returns
// ok=true once a separate "consumed" flag flips), which would silently
// disable the readloop's fast-path skip and reintroduce the per-event
// goroutine churn the issue describes.
func TestLinker_Query_R241PERF5_CacheFastPath(t *testing.T) {
	t.Parallel()
	const sessionID = "r241-perf5-cache-uuid-cccccccccc"
	l, subagentDir := newLinkerForTest(t, sessionID)
	now := time.Now()
	writeAgentFiles(t, subagentDir, "deadbeefdeadbeef0", "fastpath-1", sessionID, "p1", now)
	toolUseTime := now.Add(-50 * time.Millisecond).UnixMilli()

	const taskID = "t_fastpath"
	// Prime the cache via a regular Resolve so byTaskID has the entry.
	if _, ok := l.Resolve(context.Background(), taskID, "toolu_F", "fastpath-1", toolUseTime); !ok {
		t.Fatalf("Resolve did not converge; cache not primed")
	}
	// Query must return the cached entry with Resolved=true so the
	// readloop call site short-circuits before goroutine spawn.
	info, ok := l.Query(taskID)
	if !ok {
		t.Fatalf("Query: cached entry must surface ok=true after Resolve")
	}
	if !info.Resolved {
		t.Fatalf("Query: cached entry must surface Resolved=true (got %+v)", info)
	}
	if info.InternalAgentID == "" {
		t.Fatalf("Query: cached entry missing InternalAgentID (got %+v)", info)
	}
}
