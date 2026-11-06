package cron

import (
	"crypto/sha256"
	"log/slog"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/textutil"
)

// resultScan is how many of a job's runs are read at a time for its latest
// successful one in a window and the one the user saw before it.
const resultScan = 20

// firstLineRunes caps a result's first line.
const firstLineRunes = 80

// ResultChange is a job's latest successful run that ended in a window:
// whether its result differs from the one the user saw before the window,
// and its first line.
type ResultChange struct {
	JobID     string
	RunID     string
	EndedAt   time.Time
	Changed   bool
	FirstLine string
}

// LatestResultChange reports jobID's latest successful run that ended in
// [since, until), compared by the hash of its normalized result (whitespace
// folded) with the latest successful run that ended before since, the one
// the previous brief saw; with none, it is a first run, a change. It only
// reads the run history. false when no successful run ended in the window.
func (s *Scheduler) LatestResultChange(jobID string, since, until time.Time) (ResultChange, bool) {
	list := func(before time.Time) []CronRunSummary { return s.ListRuns(jobID, resultScan, before) }
	return latestResultChange(jobID, list, s.Run, since, until)
}

// latestResultChange is LatestResultChange over list, a job's runs started
// before a time, newest first, reading a run's result through get.
func latestResultChange(jobID string, list func(before time.Time) []CronRunSummary, get func(jobID, runID string) (*CronRun, error), since, until time.Time) (ResultChange, bool) {
	// newest is the newest readable successful run in runs that ended in
	// [from, to); stop ends the search at an older one.
	newest := func(runs []CronRunSummary, from, to time.Time, stop bool) *CronRun {
		for _, r := range runs {
			if r.State != RunStateSucceeded || !r.EndedAt.Before(to) {
				continue
			}
			if r.EndedAt.Before(from) {
				if stop {
					return nil
				}
				continue
			}
			run, err := get(jobID, r.RunID)
			if err != nil {
				slog.Debug("cron: run unreadable for its result", "job", jobID, "run", r.RunID, "err", err)
				continue
			}
			return run
		}
		return nil
	}
	runs := list(until)
	latest := newest(runs, since, until, true)
	if latest == nil {
		return ResultChange{}, false
	}
	seen := newest(runs, time.Time{}, since, false)
	if seen == nil {
		// Runs since the window opened can fill the page; the earlier
		// ones started before since.
		seen = newest(list(since), time.Time{}, since, false)
	}
	return ResultChange{JobID: jobID, RunID: latest.RunID, EndedAt: latest.EndedAt,
		Changed: seen == nil || resultHash(seen.Result) != resultHash(latest.Result), FirstLine: firstLine(latest.Result)}, true
}

// resultHash hashes a result with its whitespace folded, so a reflowed
// report is the same one.
func resultHash(s string) [32]byte {
	return sha256.Sum256([]byte(strings.Join(strings.Fields(s), " ")))
}

// firstLine is s's first non-blank line, capped at firstLineRunes.
func firstLine(s string) string {
	for line := range strings.Lines(s) {
		if line = strings.TrimSpace(line); line != "" {
			return textutil.TruncateRunes(line, firstLineRunes)
		}
	}
	return ""
}
