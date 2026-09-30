package cron

import "testing"

// fatalRecorder captures a Fatalf instead of ending the test, so the ports'
// failure paths can be asserted.
type fatalRecorder struct {
	testing.TB
	failed bool
}

type fatalSentinel struct{}

func (f *fatalRecorder) Helper() {}
func (f *fatalRecorder) Fatalf(string, ...any) {
	f.failed = true
	panic(fatalSentinel{})
}

func failsOn(t *testing.T, fn func(tb testing.TB)) bool {
	t.Helper()
	rec := &fatalRecorder{TB: t}
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(fatalSentinel); !ok {
					panic(r)
				}
			}
		}()
		fn(rec)
	}()
	return rec.failed
}

// putJobForTest indexes the job like a load does; jobForTest and
// editJobForTest fail the test on an unregistered id instead of handing back
// a zero Job or silently doing nothing.
func TestTablePorts(t *testing.T) {
	s := NewScheduler(SchedulerConfig{}, SchedulerDeps{})
	s.putJobForTest(&Job{ID: "a", Platform: "p", ChatID: "c", Prompt: "x"})
	key := chatKeyFor("p", "c")
	if s.tbl.countForChat(key) != 1 || len(s.tbl.forChat(key)) != 1 || len(s.tbl.ids()) != 1 {
		t.Errorf("putJobForTest left the indices out of step with the map")
	}
	s.editJobForTest(t, "a", func(j *Job) { j.Prompt = "y" })
	if got := s.jobForTest(t, "a"); got.Prompt != "y" || got.ID != "a" {
		t.Errorf("jobForTest after edit = %+v", got)
	}
	if !failsOn(t, func(tb testing.TB) { s.jobForTest(tb, "missing") }) {
		t.Error("jobForTest on a missing job did not fail the test")
	}
	if !failsOn(t, func(tb testing.TB) { s.editJobForTest(tb, "missing", func(*Job) {}) }) {
		t.Error("editJobForTest on a missing job did not fail the test")
	}
	s.dropJobForTest("a")
	if s.tbl.exists("a") || s.tbl.countForChat(key) != 0 {
		t.Error("dropJobForTest left the job or its index behind")
	}
}
