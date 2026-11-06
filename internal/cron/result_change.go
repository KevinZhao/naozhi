package cron

import (
	"crypto/sha256"
	"log/slog"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/textutil"
)

// resultScan is how many of a job's latest runs are read for its two latest
// successful ones.
const resultScan = 20

// firstLineRunes caps a result's first line.
const firstLineRunes = 80

// ResultChange is a job's latest successful run that ended in a window:
// whether its result differs from the successful run before it, and its
// first line.
type ResultChange struct {
	JobID     string
	RunID     string
	EndedAt   time.Time
	Changed   bool
	FirstLine string
}

// LatestResultChange reports jobID's latest successful run that ended in
// [since, until), compared with the successful run before it by the hash of
// its normalized result (whitespace folded); a first one is a change. It
// only reads the run history. false when no successful run ended in the
// window.
func (s *Scheduler) LatestResultChange(jobID string, since, until time.Time) (ResultChange, bool) {
	return latestResultChange(jobID, s.ListRuns(jobID, resultScan, time.Time{}), s.Run, since, until)
}

// latestResultChange is LatestResultChange over runs, newest first, reading a
// run's result through get.
func latestResultChange(jobID string, runs []CronRunSummary, get func(jobID, runID string) (*CronRun, error), since, until time.Time) (ResultChange, bool) {
	var latest *CronRun
	for _, r := range runs {
		if r.State != RunStateSucceeded {
			continue
		}
		if latest == nil {
			if r.EndedAt.Before(since) {
				return ResultChange{}, false
			}
			if !r.EndedAt.Before(until) {
				continue
			}
		}
		run, err := get(jobID, r.RunID)
		if err != nil {
			slog.Debug("cron: run unreadable for its result", "job", jobID, "run", r.RunID, "err", err)
			continue
		}
		if latest == nil {
			latest = run
			continue
		}
		return ResultChange{JobID: jobID, RunID: latest.RunID, EndedAt: latest.EndedAt,
			Changed: resultHash(run.Result) != resultHash(latest.Result), FirstLine: firstLine(latest.Result)}, true
	}
	if latest == nil {
		return ResultChange{}, false
	}
	return ResultChange{JobID: jobID, RunID: latest.RunID, EndedAt: latest.EndedAt, Changed: true, FirstLine: firstLine(latest.Result)}, true
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
