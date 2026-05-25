package cli

import "testing"

// TestEventLog_LastResponseSummary_Append locks the per-Append store contract
// for the R110-P1 sidebar response preview. Only EventEntry.Type == "text"
// (assistant text content blocks, as produced by EventEntriesFromEventAt)
// updates the cached summary; user / activity / system events leave the
// pointer untouched so a turn that's still mid-flight (only thinking +
// tool_use streamed so far) does not clobber the previous turn's response
// preview with empty text.
func TestEventLog_LastResponseSummary_Append(t *testing.T) {
	t.Parallel()

	l := NewEventLog(10)
	if got := l.LastResponseSummary(); got != "" {
		t.Fatalf("initial summary = %q, want \"\"", got)
	}

	// Non-text entries must NOT touch lastResponseSummary.
	l.Append(EventEntry{Type: "user", Summary: "what is 2+2?"})
	l.Append(EventEntry{Type: "thinking", Summary: "let me see"})
	l.Append(EventEntry{Type: "tool_use", Summary: "Read"})
	if got := l.LastResponseSummary(); got != "" {
		t.Errorf("after non-text appends summary = %q, want \"\"", got)
	}

	// First assistant text reply lands.
	l.Append(EventEntry{Type: "text", Summary: "the answer is 4"})
	if got, want := l.LastResponseSummary(), "the answer is 4"; got != want {
		t.Errorf("after first text summary = %q, want %q", got, want)
	}

	// Result event (turn boundary) leaves the summary intact — sidebar
	// should keep showing the most recent assistant reply, not blank out
	// when the turn closes.
	l.Append(EventEntry{Type: "result", Summary: "done"})
	if got, want := l.LastResponseSummary(), "the answer is 4"; got != want {
		t.Errorf("after result summary = %q, want %q (must survive turn close)", got, want)
	}

	// Newer assistant text replaces the cache.
	l.Append(EventEntry{Type: "user", Summary: "what about 3+3?"})
	l.Append(EventEntry{Type: "text", Summary: "six"})
	if got, want := l.LastResponseSummary(), "six"; got != want {
		t.Errorf("after second text summary = %q, want %q", got, want)
	}
}

// TestEventLog_LastResponseSummary_AppendBatch verifies the batched write
// path used by InjectHistory's 500-entry replay matches Append's semantics:
// only the LAST text entry in the batch wins (single Store under l.mu so
// concurrent Snapshot sees the batch atomically). Activity-only batches
// must not touch the cache.
func TestEventLog_LastResponseSummary_AppendBatch(t *testing.T) {
	t.Parallel()

	l := NewEventLog(20)

	// Batch with multiple text entries: only the trailing one wins.
	l.AppendBatch([]EventEntry{
		{Type: "user", Summary: "ping"},
		{Type: "text", Summary: "first reply"},
		{Type: "thinking", Summary: "..."},
		{Type: "text", Summary: "second reply"},
	})
	if got, want := l.LastResponseSummary(), "second reply"; got != want {
		t.Errorf("after multi-text batch summary = %q, want %q", got, want)
	}

	// Activity-only batch must not blank the cache.
	l.AppendBatch([]EventEntry{
		{Type: "thinking", Summary: "more"},
		{Type: "tool_use", Summary: "Grep"},
	})
	if got, want := l.LastResponseSummary(), "second reply"; got != want {
		t.Errorf("after activity-only batch summary = %q, want %q (must not be cleared)", got, want)
	}
}
