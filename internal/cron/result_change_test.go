package cron

import (
	"io/fs"
	"testing"
	"time"
)

// A job's latest successful run in the window is compared with the
// successful run before it by its normalized result.
func TestLatestResultChange(t *testing.T) {
	since := time.Date(2026, 11, 5, 8, 30, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)
	run := func(id string, at time.Time, state RunState, result string) *CronRun {
		return &CronRun{RunID: id, JobID: "j", State: state, StartedAt: at.Add(-time.Minute), EndedAt: at, Result: result}
	}
	tests := []struct {
		name        string
		runs        []*CronRun // newest first
		ok, changed bool
		first       string
	}{
		{"same result, reflowed", []*CronRun{
			run("r2", since.Add(time.Hour), RunStateSucceeded, "账单：\n  $120  "),
			run("r1", since.Add(-time.Hour), RunStateSucceeded, "账单： $120"),
		}, true, false, "账单："},
		{"result changed", []*CronRun{
			run("r2", since.Add(time.Hour), RunStateSucceeded, "账单： $130"),
			run("r1", since.Add(-time.Hour), RunStateSucceeded, "账单： $120"),
		}, true, true, "账单： $130"},
		{"a failure in between is skipped", []*CronRun{
			run("r3", since.Add(2*time.Hour), RunStateSucceeded, "ok"),
			run("r2", since.Add(time.Hour), RunStateFailed, "boom"),
			run("r1", since.Add(-time.Hour), RunStateSucceeded, "ok"),
		}, true, false, "ok"},
		{"the first run is a change", []*CronRun{run("r1", since.Add(time.Hour), RunStateSucceeded, "\n首次\n")}, true, true, "首次"},
		{"none in the window", []*CronRun{run("r1", since.Add(-time.Hour), RunStateSucceeded, "ok")}, false, false, ""},
		{"only failures", []*CronRun{run("r1", since.Add(time.Hour), RunStateFailed, "boom")}, false, false, ""},
		{"after the window", []*CronRun{
			run("r3", until.Add(time.Hour), RunStateSucceeded, "later"),
			run("r2", since.Add(time.Hour), RunStateSucceeded, "ok"),
			run("r1", since.Add(-time.Hour), RunStateSucceeded, "ok"),
		}, true, false, "ok"},
	}
	for _, tt := range tests {
		byID := map[string]*CronRun{}
		var sums []CronRunSummary
		for _, r := range tt.runs {
			byID[r.RunID] = r
			sums = append(sums, CronRunSummary{RunID: r.RunID, State: r.State, StartedAt: r.StartedAt, EndedAt: r.EndedAt})
		}
		get := func(_, id string) (*CronRun, error) {
			if r, ok := byID[id]; ok {
				return r, nil
			}
			return nil, fs.ErrNotExist
		}
		got, ok := latestResultChange("j", sums, get, since, until)
		if ok != tt.ok || got.Changed != tt.changed || got.FirstLine != tt.first {
			t.Errorf("%s: = %+v, %v; want ok %v, changed %v, first line %q", tt.name, got, ok, tt.ok, tt.changed, tt.first)
		}
	}
}
