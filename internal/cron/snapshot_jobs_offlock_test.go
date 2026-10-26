package cron

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// #1923: recordTerminalResult runs json.Marshal off s.tbl.mu, which is only
// safe because the snapshot is a detached value copy.

// marshalJobsSnapshotForTest encodes a detached snapshot through the same
// serializer persistSnapshot uses, returning the bytes directly so tests can
// assert on-disk shape without driving the async save closure.
func (s *Scheduler) marshalJobsSnapshotForTest(snap jobsSnapshot) ([]byte, error) {
	if fn := s.marshalJobs.Load(); fn != nil {
		return (*fn)(snap.entries)
	}
	return defaultMarshalJobs(snap.entries)
}

// TestSnapshotJobsForSaveLocked_Detached verifies the snapshot is a value copy
// (Notify deep-copied too) that does NOT observe a mutation applied after
// s.tbl.mu is released. This is the correctness guarantee that lets
// json.Marshal run off the lock without racing a concurrent mutator.
func TestSnapshotJobsForSaveLocked_Detached(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	notify := true
	j := &Job{ID: "abcd1234", Schedule: "@every 1h", Prompt: "original", Platform: "feishu", ChatID: "c1", ChatType: "direct", Paused: true, Notify: &notify}
	s.putJobForTest(j)
	snap := s.snapshotForSaveForTest()

	// Mutate the live job AFTER the snapshot was taken and the lock dropped.
	j.Prompt = "MUTATED"
	*j.Notify = false

	data, err := s.marshalJobsSnapshotForTest(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var rt []*Job
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, data)
	}
	if len(rt) != 1 {
		t.Fatalf("want 1 job, got %d", len(rt))
	}
	if rt[0].Prompt != "original" {
		t.Fatalf("snapshot leaked post-unlock mutation: Prompt=%q want %q", rt[0].Prompt, "original")
	}
	if rt[0].Notify == nil || !*rt[0].Notify {
		t.Fatalf("snapshot Notify = %v, want true (Notify must be deep-copied)", rt[0].Notify)
	}
}
