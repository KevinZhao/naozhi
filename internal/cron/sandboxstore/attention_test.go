package sandboxstore

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestGetAttention_OversizeIsRefused: the record carries a handful of ids and a
// label, so a multi-megabyte one is a tampered or runaway file and must not be
// allocated.
func TestGetAttention_OversizeIsRefused(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	dir := st.attentionDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	runID := hexID(t)
	// Valid JSON, just far too big: padding rides in an unknown field, which
	// encoding/json ignores. A payload that is merely malformed would be
	// refused as corrupt even with no cap at all, so it could not tell whether
	// the cap is doing anything.
	pad := make([]byte, maxAttentionRecordBytes)
	for i := range pad {
		pad[i] = 'x'
	}
	body := `{"run_id":"` + runID + `","job_id":"0123456789abcdef","pad":"` + string(pad) + `"}`
	if err := os.WriteFile(filepath.Join(dir, runID+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := st.GetAttention(runID); err == nil || ok {
		t.Error("an over-cap record must be refused rather than parsed")
	}
}

// TestZeroStore_PersistsNothing: the zero Store is what a scheduler without a
// store path holds. Every writer must no-op and every reader report absent,
// without touching the working directory.
func TestZeroStore_PersistsNothing(t *testing.T) {
	t.Chdir(t.TempDir())
	var st Store
	jobID, runID := hexID(t), hexID(t)

	st.WriteAttention(Attention{JobID: jobID, RunID: runID, Reason: ReasonTransport}, slog.Default())
	st.WriteSnapshot(jobID, runID, "prompt", "", "", nil, slog.Default())
	sink, closeSink := st.EventSink(jobID, runID, slog.Default())
	if err := sink([]byte(`{"k":"v"}`)); err != nil {
		t.Fatal(err)
	}
	closeSink()
	st.GCBlobs()
	st.DeleteJobAttention(jobID)
	st.DeleteJobSnapshots(jobID)
	st.DeleteJobEvents(jobID)

	if rec, ok, err := st.GetAttention(runID); rec != nil || ok || err != nil {
		t.Errorf("GetAttention = (%v, %v, %v), want absent", rec, ok, err)
	}
	if err := st.RemoveAttention(runID); err != nil {
		t.Errorf("RemoveAttention = %v", err)
	}
	if got := st.ListAttention(); got == nil || len(got) != 0 {
		t.Errorf("ListAttention = %#v, want an empty non-nil slice", got)
	}
	if n := st.AttentionCount(); n != 0 {
		t.Errorf("AttentionCount = %d", n)
	}
	if man, ok, err := st.SnapshotManifest(jobID, runID); man != nil || ok || err != nil {
		t.Errorf("SnapshotManifest = (%v, %v, %v), want absent", man, ok, err)
	}
	if p, err := st.SnapshotPrompt("00"); p != "" || err != nil {
		t.Errorf("SnapshotPrompt = (%q, %v), want empty", p, err)
	}
	if lines, trunc, err := st.RunEvents(jobID, runID, 10); lines != nil || trunc || err != nil {
		t.Errorf("RunEvents = (%v, %v, %v), want empty", lines, trunc, err)
	}
	if entries, _ := os.ReadDir("."); len(entries) != 0 {
		t.Errorf("the zero Store wrote %d entries into the working directory", len(entries))
	}
}
