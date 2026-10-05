package cron

// jobtable_state.go — the table's remaining lock holders: the startup load,
// a run's terminal result, the shutdown persist, and the two lookups
// NextRun and TriggerNow make. Like the write side, each hands back data;
// robfig and the disk are the caller's, off the lock.

import (
	"fmt"
	"log/slog"
	"time"
)

// loadResult is what load registered: the entries to commit (active jobs
// only), the stubs to refresh, and how many jobs each cap turned away.
type loadResult struct {
	plans              []cronEntryPlan
	stubs              []jobStubFields
	loaded             int
	skippedOverCap     int
	skippedOverPerChat int
}

// load registers restored jobs within the global and per-chat caps, skipping
// any whose schedule no longer plans. Over-cap jobs stay on disk (Start never
// persists), so raising the cap and restarting recovers them (#1187, #2060).
func (t *jobTable) load(restored []*Job, maxJobs, maxPerChat int) loadResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	var r loadResult
	for _, j := range restored {
		if len(t.jobs) >= maxJobs {
			slog.Warn("cron job over maxJobs cap; skipping (raise cron.MaxJobs to restore)",
				"job_id", j.ID, "schedule", j.Schedule, "cap", maxJobs)
			r.skippedOverCap++
			continue
		}
		if t.chatJobCount[chatKeyFor(j.Platform, j.ChatID)] >= maxPerChat {
			slog.Warn("cron job over per-chat cap; skipping (raise cron.MaxJobsPerChat to restore)",
				"job_id", j.ID, "platform", j.Platform, "chat_id", j.ChatID, "cap", maxPerChat)
			r.skippedOverPerChat++
			continue
		}
		if !j.Paused {
			p, err := planCronEntry(j.ID, j.Schedule, time.Now())
			if err != nil {
				slog.Warn("skip invalid cron job", "job_id", j.ID, "schedule", j.Schedule, "err", err)
				continue
			}
			r.plans = append(r.plans, p)
		}
		t.jobs[j.ID] = j
		t.indexLocked(j)
		// lastSessionID 一起快照，重启后恢复的 cron stub 才能带上最近一次执行留下的
		// session_id，historySource 才能从 JSONL 把历史读回来给 dashboard 显示。
		r.stubs = append(r.stubs, jobStubFields{id: j.ID, workDir: j.WorkDir, prompt: j.Prompt, lastSessionID: j.LastSessionID})
	}
	r.loaded = len(t.jobs)
	return r
}

// terminalRecord is a run's already-sanitised terminal result.
type terminalRecord struct {
	endedAt   time.Time
	result    string
	errMsg    string
	sessionID string
	errClass  ErrorClass
	turnCause TurnCause
	state     RunState
}

// resultChange is what recordResult did: the detached snapshot to marshal off
// the lock (#1923), the state to restore if that fails, and whether the job's
// session id moved.
type resultChange struct {
	snap           jobsSnapshot
	prev           JobState
	sessionChanged bool
}

// recordResult writes rec onto job jobID. ok is false when the job was deleted
// since the run started; nothing changed then.
func (t *jobTable) recordResult(jobID string, rec terminalRecord) (c resultChange, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[jobID]
	if !ok {
		return resultChange{}, false
	}
	c.prev = j.snapshotResultState()
	j.LastRunAt = rec.endedAt
	j.LastResult = rec.result
	j.LastError = rec.errMsg
	j.LastErrorClass = rec.errClass
	if rec.sessionID != "" {
		j.LastSessionID = rec.sessionID
	}
	j.RunCounters.addRun(rec.state)
	j.ConsecutiveFailures = nextFailureStreak(j.ConsecutiveFailures, rec.state, rec.errClass, rec.turnCause)
	c.snap = t.snapshotForSaveLocked()
	c.sessionChanged = rec.sessionID != "" && rec.sessionID != c.prev.LastSessionID
	return c, true
}

// revertResult restores the terminal-result state recordResult replaced, after
// its snapshot failed to marshal. A job deleted in between is left alone.
func (t *jobTable) revertResult(jobID string, prev JobState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if j, ok := t.jobs[jobID]; ok {
		prev.restore(j)
	}
}

// persist marshals the whole job set under its own hold of t.mu; the shutdown
// save uses it, and the seq it returns is the one the save must land.
func (t *jobTable) persist() (marshaledJobs, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.persistLocked()
}

// entryIDOf returns j's cron entry id, read under t.mu. j may be a detached
// copy with no entry (a deserialised snapshot), so an unset id falls back to
// the registered job of the same ID (#784).
func (t *jobTable) entryIDOf(j *Job) cronEntryID {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if j.entryID != 0 || j.ID == "" {
		return j.entryID
	}
	if live, ok := t.jobs[j.ID]; ok {
		return live.entryID
	}
	return 0
}

// triggerable reports whether a manual run of job id may start: the job
// exists, is not paused, and has a prompt.
func (t *jobTable) triggerable(id string) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	j, ok := t.jobs[id]
	switch {
	case !ok:
		return fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	case j.Paused:
		return fmt.Errorf("%w: id %q", ErrJobPaused, id)
	case j.Prompt == "":
		return fmt.Errorf("%w: id %q", ErrJobNoPrompt, id)
	}
	return nil
}
