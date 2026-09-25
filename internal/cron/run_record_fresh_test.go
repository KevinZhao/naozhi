package cron

import "testing"

// TestLocalRun_RecordsFreshSnapshotThroughFinishRun pins CronRun.Fresh on the
// normal execute -> finishRun path, where nothing else checks it: the field
// travels in runCtx.snap, and hard-wiring it to false in that composition
// otherwise leaves the whole package green (#2742).
//
// Both polarities: a record that always says true (or always false) passes a
// single-sided check.
func TestLocalRun_RecordsFreshSnapshotThroughFinishRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    string
		fresh bool
	}{
		{"fresh context", "00000000000f0e51", true},
		{"persistent context", "00000000000f0e50", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newCostScheduler(t, costRouter{sess: &costSession{cumulative: []float64{0.1}}})
			j := &Job{ID: tc.id, Schedule: "@every 5m", Prompt: "ping", WorkDir: "/home/u/proj", FreshContext: tc.fresh}
			s.tblForTest().mu.Lock()
			s.tblForTest().jobs[j.ID] = j
			s.tblForTest().mu.Unlock()

			s.executeOpt(j, true)

			runs := s.RecentRuns(j.ID, 5)
			if len(runs) != 1 {
				t.Fatalf("runs = %d, want 1", len(runs))
			}
			// The summary does not carry Fresh; read the full record back from
			// disk, which is what a replay and the dashboard detail view see.
			rec, err := s.Run(j.ID, runs[0].RunID)
			if err != nil || rec == nil {
				t.Fatalf("Run(%s): rec=%v err=%v", runs[0].RunID, rec, err)
			}
			if rec.Fresh != tc.fresh {
				t.Errorf("CronRun.Fresh = %v, want %v — the fresh snapshot was lost between execute and the record", rec.Fresh, tc.fresh)
			}
			if rec.WorkDir != j.WorkDir {
				t.Errorf("CronRun.WorkDir = %q, want %q", rec.WorkDir, j.WorkDir)
			}
		})
	}
}
