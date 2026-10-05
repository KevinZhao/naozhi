package cron

// jobtable_edit.go — the in-lock half of AddJob, UpdateJob and SetJobPrompt.
// Each validates, mutates, and persists in one hold of t.mu, undoes the
// mutation itself when the persist fails, and hands back as data what the
// caller must do after the lock is released: commit a cron entry, retire
// one, write the snapshot, refresh the router stub.

import (
	"fmt"
	"log/slog"
	"time"
)

// jobStubFields is the lock-held snapshot of what the router stub refresh
// reads, so a concurrent UpdateJob / SetJobPrompt cannot change it under the
// caller after the lock is released (#1068).
type jobStubFields struct {
	id            string
	workDir       string
	prompt        string
	lastSessionID string
}

// insertResult is what insert did: plan is non-nil for an active job and must
// be committed by the caller after the lock is released.
type insertResult struct {
	plan *cronEntryPlan
	stub jobStubFields
	snap marshaledJobs
}

// insert registers j under a fresh ID, within the global and per-chat caps.
// The cron entry is only planned: a persist failure then has no robfig entry
// to roll back, and deleteLocked under the same hold undoes everything
// (#1810). The router stub is registered by the caller after the save, so no
// router cleanup is needed either.
func (t *jobTable) insert(j *Job, maxJobs, maxPerChat int) (insertResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.jobs) >= maxJobs {
		return insertResult{}, fmt.Errorf("%w: max cron jobs reached (%d)", ErrJobQuotaExceeded, maxJobs)
	}
	// Per-chat limit so one chat cannot exhaust the global quota. O(1) via
	// chatJobCount, kept in lock-step with jobs by indexLocked / deleteLocked (#661).
	if t.chatJobCount[chatKeyFor(j.Platform, j.ChatID)] >= maxPerChat {
		return insertResult{}, fmt.Errorf("%w: per-chat cron limit reached (%d)", ErrJobQuotaExceeded, maxPerChat)
	}
	id, err := t.freshIDLocked()
	if err != nil {
		return insertResult{}, err
	}
	j.ID = id
	j.CreatedAt = time.Now()

	var r insertResult
	if !j.Paused {
		// Plan only — parse the schedule and derive the cache — so a bad spec is
		// rejected before anything mutates.
		p, perr := planCronEntry(j.ID, j.Schedule, time.Now())
		if perr != nil {
			return insertResult{}, perr
		}
		r.plan = &p
	}
	t.jobs[j.ID] = j
	t.indexLocked(j)
	if r.snap, err = t.persistLocked(); err != nil {
		t.deleteLocked(j)
		return insertResult{}, err
	}
	r.stub = jobStubFields{id: j.ID, workDir: j.WorkDir, prompt: j.Prompt, lastSessionID: j.LastSessionID}
	return r, nil
}

// freshIDLocked returns an ID no registered job has. Retries on a collision,
// bounded so a degenerate generator cannot spin under t.mu. Warns once on the
// first collision; the same ID twice in a row proves a deterministic
// generator, so it bails at Error (#493).
func (t *jobTable) freshIDLocked() (string, error) {
	id, err := generateID()
	if err != nil {
		// crypto/rand 失败透传：AddJob 是公共入口，应表现为请求拒绝而非 panic；
		// rand 整体失效时重试只会复现同一错误，提早 bail (#706)。
		return "", fmt.Errorf("cron: generate job id: %w", err)
	}
	prevID := id
	for i := 0; i < 10; i++ {
		if _, exists := t.jobs[id]; !exists {
			break
		}
		if i == 0 {
			slog.Warn("cron: job ID collision, retrying", "attempt", i+1, "job_id", id)
		}
		retryID, retryErr := generateID()
		if retryErr != nil {
			// 同上：rand 中途失效，提早返回比继续循环更诚实。
			return "", fmt.Errorf("cron: regenerate job id (retry %d): %w", i+1, retryErr)
		}
		if retryID == prevID {
			slog.Error("cron: deterministic ID generator detected; bailing early",
				"attempt", i+1, "id", retryID)
			return "", fmt.Errorf("cron: deterministic ID generator (id %q repeated)", retryID)
		}
		prevID = retryID
		id = retryID
	}
	if _, exists := t.jobs[id]; exists {
		return "", fmt.Errorf("cron: failed to generate unique job ID after 10 attempts")
	}
	return id, nil
}

// reschedule is an UpdateJob schedule change on an active job: the entry the
// caller must Remove, and the schedules to plan (new) or fall back to (old).
type reschedule struct {
	removeEntry cronEntryID
	oldSchedule string
	newSchedule string
}

// updateResult is the job as the edit left it, and the entry swap the caller
// must perform when resched is non-nil.
type updateResult struct {
	job     Job
	resched *reschedule
	snap    marshaledJobs
}

// update applies upd to job id, or with upd.InChat to the job findByPrefixLocked
// resolves id to in that chat. Every failure — the sandbox+work_dir guard on
// the effective post-patch job (agentcore §4.4) or the persist — restores the
// pre-update job by value under the same hold, including the Notify pointer
// applyTo replaces. The schedule is not applyTo's: it is written here, and on
// an active job its entry and cache are zeroed so readers see entryID=0 at
// once; the swap itself is the caller's, off the lock.
func (t *jobTable) update(id string, upd JobUpdate) (updateResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	j := t.jobs[id]
	if c := upd.InChat; c != nil {
		var err error
		if j, err = t.findByPrefixLocked(id, c.Platform, c.ChatID); err != nil {
			return updateResult{}, err
		}
	} else if j == nil {
		return updateResult{}, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	preUpdate := *j
	upd.applyTo(j)
	// An edit is the user's attempt at a fix: the failure streaks start over.
	j.setStreaks(failureStreaks{})
	if placementIsSandbox(j.Placement) && j.WorkDir != "" {
		*j = preUpdate
		return updateResult{}, ErrSandboxWorkDir
	}
	var r updateResult
	if upd.Schedule != nil && *upd.Schedule != j.Schedule {
		j.Schedule = *upd.Schedule
		if !j.Paused {
			r.resched = &reschedule{removeEntry: j.entryID, oldSchedule: preUpdate.Schedule, newSchedule: j.Schedule}
			j.entryID = 0
			j.cachedPeriod = 0
			j.cachedSched = nil
		}
	}
	snap, err := t.persistLocked()
	if err != nil {
		*j = preUpdate
		return updateResult{}, err
	}
	r.job, r.snap = *j, snap
	return r, nil
}

// revertSchedule puts job id back on oldSchedule after its new schedule failed
// to plan, marking it Paused when even the old one cannot fire, and persists
// that. ok is false when the job is gone.
func (t *jobTable) revertSchedule(id, oldSchedule string, pause bool) (snap marshaledJobs, ok bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	if !ok {
		return marshaledJobs{}, false, nil
	}
	j.Schedule = oldSchedule
	if pause {
		j.Paused = true
	}
	snap, err = t.persistLocked()
	return snap, true, err
}

// fillResult is what fillPrompt did: plan is non-nil when the fill resumed a
// paused job and must be committed by the caller after the lock is released.
type fillResult struct {
	plan *cronEntryPlan
	stub jobStubFields
	snap marshaledJobs
}

// fillPrompt sets job id's first prompt, resuming it if it was paused. It
// never overwrites: a job that already has a prompt returns
// ErrPromptAlreadySet unchanged (#1503). A persist failure restores the empty
// prompt and the pause; the resume plan was never committed, so that is the
// whole rollback (#537).
func (t *jobTable) fillPrompt(id, prompt string) (fillResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	j, ok := t.jobs[id]
	if !ok {
		return fillResult{}, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	if j.Prompt != "" {
		return fillResult{}, ErrPromptAlreadySet
	}
	var r fillResult
	wasPaused := j.Paused
	prevReason, prevStreaks := j.PausedReason, j.streaks()
	if wasPaused {
		p, err := t.resumeLocked(j)
		if err != nil {
			return fillResult{}, err
		}
		r.plan = &p
	}
	j.Prompt = prompt
	snap, err := t.persistLocked()
	if err != nil {
		j.Prompt, j.Paused = "", wasPaused
		j.PausedReason = prevReason
		j.setStreaks(prevStreaks)
		return fillResult{}, err
	}
	r.snap = snap
	r.stub = jobStubFields{id: j.ID, workDir: j.WorkDir, prompt: prompt, lastSessionID: j.LastSessionID}
	return r, nil
}
