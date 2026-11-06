package cron

import (
	"io/fs"
	"testing"
	"time"
)

// A job's latest successful run in the window is compared by its normalized
// result with the latest successful run before the window.
func TestLatestResultChange(t *testing.T) {
	since := time.Date(2026, 11, 5, 8, 30, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)
	run := func(id string, at time.Time, state RunState, result string) *CronRun {
		return &CronRun{RunID: id, JobID: "j", State: state, StartedAt: at.Add(-time.Minute), EndedAt: at, Result: result}
	}
	tests := []struct {
		name        string
		runs        []*CronRun // newest first
		page        int        // runs per list call; 0 for all
		ok, changed bool
		first       string
	}{
		{"same result, reflowed", []*CronRun{
			run("r2", since.Add(time.Hour), RunStateSucceeded, "账单：\n  $120  "),
			run("r1", since.Add(-time.Hour), RunStateSucceeded, "账单： $120"),
		}, 0, true, false, "账单："},
		{"result changed", []*CronRun{
			run("r2", since.Add(time.Hour), RunStateSucceeded, "账单： $130"),
			run("r1", since.Add(-time.Hour), RunStateSucceeded, "账单： $120"),
		}, 0, true, true, "账单： $130"},
		{"a failure in between is skipped", []*CronRun{
			run("r3", since.Add(2*time.Hour), RunStateSucceeded, "ok"),
			run("r2", since.Add(time.Hour), RunStateFailed, "boom"),
			run("r1", since.Add(-time.Hour), RunStateSucceeded, "ok"),
		}, 0, true, false, "ok"},
		{"the first run is a change", []*CronRun{run("r1", since.Add(time.Hour), RunStateSucceeded, "\n首次\n")}, 0, true, true, "首次"},
		{"none in the window", []*CronRun{run("r1", since.Add(-time.Hour), RunStateSucceeded, "ok")}, 0, false, false, ""},
		{"only failures", []*CronRun{run("r1", since.Add(time.Hour), RunStateFailed, "boom")}, 0, false, false, ""},
		{"after the window", []*CronRun{
			run("r3", until.Add(time.Hour), RunStateSucceeded, "later"),
			run("r2", since.Add(time.Hour), RunStateSucceeded, "ok"),
			run("r1", since.Add(-time.Hour), RunStateSucceeded, "ok"),
		}, 0, true, false, "ok"},
		{"changed early in the window", []*CronRun{
			run("r3", since.Add(20*time.Hour), RunStateSucceeded, "A"),
			run("r2", since.Add(10*time.Hour), RunStateSucceeded, "A"),
			run("r1", since.Add(-time.Hour), RunStateSucceeded, "B"),
		}, 0, true, true, "A"},
		{"changed and back in the window", []*CronRun{
			run("r3", since.Add(20*time.Hour), RunStateSucceeded, "A"),
			run("r2", since.Add(10*time.Hour), RunStateSucceeded, "B"),
			run("r1", since.Add(-time.Hour), RunStateSucceeded, "A"),
		}, 0, true, false, "A"},
		{"the window's runs fill a page", []*CronRun{
			run("r3", since.Add(20*time.Hour), RunStateSucceeded, "A"),
			run("r2", since.Add(10*time.Hour), RunStateSucceeded, "A"),
			run("r1", since.Add(-time.Hour), RunStateSucceeded, "A"),
		}, 2, true, false, "A"},
		{"none before the window", []*CronRun{
			run("r2", since.Add(20*time.Hour), RunStateSucceeded, "A"),
			run("r1", since.Add(10*time.Hour), RunStateSucceeded, "A"),
		}, 0, true, true, "A"},
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
		list := func(before time.Time) []CronRunSummary {
			var page []CronRunSummary
			for _, r := range sums {
				if r.StartedAt.Before(before) && (tt.page == 0 || len(page) < tt.page) {
					page = append(page, r)
				}
			}
			return page
		}
		got, ok := latestResultChange("j", list, get, since, until)
		if ok != tt.ok || got.Changed != tt.changed || got.FirstLine != tt.first {
			t.Errorf("%s: = %+v, %v; want ok %v, changed %v, first line %q", tt.name, got, ok, tt.ok, tt.changed, tt.first)
		}
	}
}
