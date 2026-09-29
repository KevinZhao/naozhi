package cron

// jobtable_write.go — the registry's write side: the index maintenance that
// keeps jobs, chatJobCount, jobsByChat and sortedJobIDs in step, the
// persist snapshot taken under the same lock, and the per-job state
// transitions. Methods ending in Locked require the caller to hold t.mu.

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// indexLocked records a job into the per-chat side indexes that must
// move in lockstep with t.jobs (chatJobCount cap counter, jobsByChat lookup
// slice, sortedJobIDs). Caller MUST hold t.mu.Lock() and have already
// inserted j into t.jobs. deleteLocked is the paired inverse.
func (t *jobTable) indexLocked(j *Job) {
	key := chatKeyFor(j.Platform, j.ChatID)
	t.chatJobCount[key]++
	t.jobsByChat[key] = append(t.jobsByChat[key], j)
	t.insertSortedIDLocked(j.ID)
}

// insertSortedIDLocked keeps t.sortedJobIDs ascending via binary-search insert
// so marshalLocked can iterate without re-sorting on every persist.
// Idempotent on a duplicate ID so a malformed disk load keeps the slice 1:1
// with the map. Caller must hold t.mu.Lock() (#1598).
func (t *jobTable) insertSortedIDLocked(id string) {
	i, found := slices.BinarySearch(t.sortedJobIDs, id)
	if found {
		return
	}
	t.sortedJobIDs = slices.Insert(t.sortedJobIDs, i, id)
}

// removeSortedIDLocked drops id from t.sortedJobIDs preserving order. No-op if
// absent so a double-delete (rollback path) cannot panic. Caller must hold
// t.mu.Lock().
func (t *jobTable) removeSortedIDLocked(id string) {
	if i, found := slices.BinarySearch(t.sortedJobIDs, id); found {
		t.sortedJobIDs = slices.Delete(t.sortedJobIDs, i, i+1)
	}
}

// deleteLocked performs the in-memory side effects of removing a job:
// snapshot+zero the cron entry and drop the map/index entries. It returns the
// captured cron entryID (0 if none) so the caller runs the cron Remove AFTER
// releasing t.mu — Remove sends on the unbuffered c.remove channel that only
// run() drains, so doing it under t.mu would hold the write lock across a
// cron-select round-trip (#1810). Caller must hold t.mu.Lock().
//
// Intentionally does NOT delete from s.gate.runningJobs (a concurrent execute may
// still hold the CAS gate; see cleanupRunningJobIfIdle) and MUST NOT call
// router.Reset (its callbacks may re-take t.mu) — callers do that after unlock.
func (t *jobTable) deleteLocked(j *Job) (removeEntryID cronEntryID) {
	removeEntryID = j.entryID
	j.entryID = 0
	if _, present := t.jobs[j.ID]; present {
		delete(t.jobs, j.ID)
		// Paired removal from the sorted-ID slice, guarded by the same
		// membership check so a double-delete cannot disturb it.
		t.removeSortedIDLocked(j.ID)
		// Paired decrement for the per-chat counter; the membership guard keeps
		// a double-delete from driving it negative (which would silently disable
		// the per-chat cap). Drop the key at zero so the map tracks live chats.
		key := chatKeyFor(j.Platform, j.ChatID)
		if n := t.chatJobCount[key]; n > 1 {
			t.chatJobCount[key] = n - 1
		} else {
			delete(t.chatJobCount, key)
		}
		// Paired remove from the per-chat index. Swap-and-shrink is fine:
		// findByPrefixLocked reports ambiguity instead of picking a winner, so
		// order is irrelevant. Drop the key when the slice empties.
		if list := t.jobsByChat[key]; len(list) > 0 {
			for i, p := range list {
				if p == j {
					last := len(list) - 1
					list[i] = list[last]
					list[last] = nil // help GC drop the pointer
					list = list[:last]
					break
				}
			}
			if len(list) == 0 {
				delete(t.jobsByChat, key)
			} else {
				t.jobsByChat[key] = list
			}
		}
	}
	return removeEntryID
}

// pauseLocked transitions a job to Paused under t.mu. Returns
// ErrJobAlreadyPaused without mutation if already paused (callers map it to
// 409). The cron Remove is NOT done here: robfig's Remove sends on the
// unbuffered c.remove channel, so the captured entry id is returned for the
// caller to Remove AFTER releasing t.mu (#537); 0 means there is nothing to
// remove.
func (t *jobTable) pauseLocked(j *Job) (removeEntryID cronEntryID, err error) {
	if j.Paused {
		return 0, fmt.Errorf("%w: id %q", ErrJobAlreadyPaused, j.ID)
	}
	// Snapshot the entryID for post-unlock removal and zero it under lock so
	// concurrent ListAllJobsWithNextRun / NextRun / TriggerNow snapshots see
	// the entry-removed state before cron's own table catches up.
	captured := j.entryID
	j.entryID = 0
	j.Paused = true
	return captured, nil
}

// resumeLocked transitions a paused job back to active under t.mu and
// returns the cron-entry plan the caller must commit AFTER releasing the lock
// (commitAndApplyCronEntry). Returns ErrJobNotPaused, or a parse error, both
// without mutation.
//
// Registration is deferred past persist on purpose: a persist failure then
// happens BEFORE any entry exists, so the rollback is one field write (no
// live entry to un-register from outside the lock, #1226's double-fire), and
// the window it opens (Paused=false, entryID=0) is fenced by entryMu, which
// every caller holds.
func (t *jobTable) resumeLocked(j *Job) (cronEntryPlan, error) {
	if !j.Paused {
		return cronEntryPlan{}, fmt.Errorf("%w: id %q", ErrJobNotPaused, j.ID)
	}
	p, err := planCronEntry(j.ID, j.Schedule, time.Now())
	if err != nil {
		return cronEntryPlan{}, err
	}
	j.Paused = false
	return p, nil
}

// findByPrefixLocked finds a job by ID prefix scoped to a specific chat.
// Returns exactly one of: (job, nil) — unique match in (plat, chatID);
// (nil, ErrJobNotFound) — no match, OR a full-length ID exists in a different
// chat (masked so callers cannot probe foreign jobs by ID); (nil,
// ErrAmbiguousPrefix) — a short prefix matches ≥2 jobs; the message lists the
// colliding IDs so the operator can disambiguate (#950).
//
// LOCK: caller MUST hold t.mu (read or write). A full-length hex ID takes the
// O(1) t.jobs fast path (#705); a partial prefix scans only
// t.jobsByChat[chat], bounded by maxJobsPerChat rather than all jobs (#558).
func (t *jobTable) findByPrefixLocked(idPrefix, plat, chatID string) (*Job, error) {
	if len(idPrefix) == 2*hexIDEntropyBytes {
		if j, ok := t.jobs[idPrefix]; ok {
			if j.Platform == plat && j.ChatID == chatID {
				return j, nil
			}
			// Full ID exists but in a different chat scope — surface
			// the same NotFound error the scan path would, so cross-
			// chat callers can't probe foreign-job existence by ID.
			return nil, fmt.Errorf("%w: prefix %q", ErrJobNotFound, idPrefix)
		}
		// Full-length ID with no map hit still falls through to the scan: a
		// corrupt store or future ID-width bump could leave a 16-char prefix
		// that is not a full ID, so the scan tail is the safety net.
	}
	var matches []*Job
	for _, j := range t.jobsByChat[chatKeyFor(plat, chatID)] {
		if strings.HasPrefix(j.ID, idPrefix) {
			matches = append(matches, j)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: prefix %q", ErrJobNotFound, idPrefix)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return nil, fmt.Errorf("%w: prefix %q matches %s", ErrAmbiguousPrefix, idPrefix, strings.Join(ids, ", "))
	}
}

// marshalLocked serialises the current jobs map to JSON while the caller
// still holds t.mu. Safe because json.Marshal only reads Job fields and the
// output []byte is independent of t.jobs, so the caller can drop t.mu
// immediately. The unexported entryID never leaks into cron_jobs.json.
// The entries slice comes from marshalEntriesPool; the output bytes are fresh
// each call because saveMarshaledSeq holds them across the async storeMu write.
func (t *jobTable) marshalLocked() ([]byte, error) {
	entriesPtr := marshalEntriesPool.Get().(*[]*Job)
	defer putMarshalEntries(entriesPtr)
	entries := *entriesPtr
	// Grow when the pooled cap is below the job count; the grown slice circulates back.
	if cap(entries) < len(t.jobs) {
		entries = make([]*Job, 0, len(t.jobs))
	}
	// Emit in t.sortedJobIDs order so on-disk JSON stays deterministic without an
	// O(N log N) sort inside the t.mu critical section (#1598). t.jobs remains the
	// source of truth: if the hint drifted (a test helper poking t.jobs directly)
	// fall back to building + sorting from the map so no job is silently dropped.
	useHint := len(t.sortedJobIDs) == len(t.jobs)
	if useHint {
		for _, id := range t.sortedJobIDs {
			j, ok := t.jobs[id]
			if !ok {
				useHint = false
				break
			}
			entries = append(entries, j)
		}
	}
	if !useHint {
		// Drift fallback: rebuild from the map (authoritative) and sort. Cold in production.
		entries = entries[:0]
		for _, j := range t.jobs {
			entries = append(entries, j)
		}
		if len(entries) > 1 {
			slices.SortFunc(entries, jobIDCmpForSort)
		}
	}
	*entriesPtr = entries
	var fn *marshalJobsFn
	if t.marshal != nil {
		fn = t.marshal.Load()
	}
	if fn == nil {
		// A table built without the Scheduler's hook still uses the production
		// marshaller.
		return defaultMarshalJobs(entries)
	}
	return (*fn)(entries)
}

// snapshotForSaveLocked captures the current job set into a detached
// marshal-ready snapshot under the caller's t.mu, plus the persist seq.
// Entries are value copies so the caller can release t.mu and marshal off the
// hot lock. Job is a flat value type — the only pointer field is Notify *bool,
// which is deep-copied so the off-lock marshal cannot race a mutator; entryID /
// cachedPeriod / cachedSched are runtime-only and excluded from JSON. The
// sorted-ID hint and drift fallback mirror marshalLocked so the on-disk
// ordering is byte-identical regardless of which persist path ran.
func (t *jobTable) snapshotForSaveLocked() jobsSnapshot {
	// Pooled outer slice (same pool as marshalLocked), returned by
	// persistSnapshot after marshal (#1975).
	entriesPtr := marshalEntriesPool.Get().(*[]*Job)
	entries := *entriesPtr
	if cap(entries) < len(t.jobs) {
		entries = make([]*Job, 0, len(t.jobs))
	} else {
		entries = entries[:0]
	}
	useHint := len(t.sortedJobIDs) == len(t.jobs)
	if useHint {
		for _, id := range t.sortedJobIDs {
			j, ok := t.jobs[id]
			if !ok {
				useHint = false
				break
			}
			cp := *j
			// Deep-copy Notify so the off-lock marshal never aliases the live job.
			if j.Notify != nil {
				v := *j.Notify
				cp.Notify = &v
			}
			entries = append(entries, &cp)
		}
	}
	if !useHint {
		entries = entries[:0]
		for _, j := range t.jobs {
			cp := *j
			// Deep-copy Notify so the off-lock marshal never aliases the live job.
			if j.Notify != nil {
				v := *j.Notify
				cp.Notify = &v
			}
			entries = append(entries, &cp)
		}
		if len(entries) > 1 {
			slices.SortFunc(entries, jobIDCmpForSort)
		}
	}
	// Write the (possibly grown) slice back into the pooled handle so a grow
	// circulates through the pool.
	*entriesPtr = entries
	return jobsSnapshot{entries: entries, seq: t.saveSeq.Add(1), pooled: entriesPtr}
}

// applyEntry writes a committed robfig registration onto job p.jobID and
// reports whether the job is still registered; when it is not, the caller
// must remove the entry it just committed.
func (t *jobTable) applyEntry(p cronEntryPlan, id cronEntryID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[p.jobID]
	if ok {
		applyCronEntry(j, p, id)
	}
	return ok
}

// marshaledJobs is a persist snapshot: the marshaled job set and the seq that
// orders it against every other snapshot (saveMarshaledSeq drops one older
// than what already landed).
type marshaledJobs struct {
	data []byte
	seq  uint64
}

// persistLocked marshals the job set under the caller's t.mu and takes the
// next seq. A failure is wrapped as ErrPersistFailed with the cause, and the
// caller must undo the mutation it was persisting or surface the error: the
// in-memory state is now ahead of disk.
func (t *jobTable) persistLocked() (marshaledJobs, error) {
	data, err := t.marshalLocked()
	if err != nil {
		slog.Error("marshal cron store", "err", err)
		return marshaledJobs{}, fmt.Errorf("%w: %w", ErrPersistFailed, err)
	}
	return marshaledJobs{data: data, seq: t.saveSeq.Add(1)}, nil
}

// mutationKind is a per-job state change the dashboard and IM commands make by
// exact ID or by chat-scoped prefix.
type mutationKind int

const (
	mutDelete mutationKind = iota + 1
	mutPause
	mutResume
)

// mutationResult is what a mutation did, as data. The caller runs the robfig
// and router side effects and the disk write after the lock is released; the
// table never calls out.
type mutationResult struct {
	// lookupErr: no such job (or an ambiguous prefix). Nothing changed.
	lookupErr error
	// opErr: the transition does not apply (already paused, not paused, a bad
	// schedule). Nothing changed.
	opErr error
	// persistErr: the snapshot did not marshal. A pause or resume was undone
	// before the lock was released and carries no removeEntry or plan; a
	// delete stays done, its cleanup must still run (#1149).
	persistErr error
	// job is the job as the mutation left it (or as the rollback restored it).
	job Job
	// removeEntry is the robfig entry a delete or pause retired; plan is the
	// entry a resume must commit.
	removeEntry cronEntryID
	plan        *cronEntryPlan
	// snap is the snapshot to write when persistErr is nil.
	snap marshaledJobs
}

// mutateByID applies kind to the job with exact id.
func (t *jobTable) mutateByID(id string, kind mutationKind) mutationResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	if !ok {
		return mutationResult{lookupErr: fmt.Errorf("%w: id %q", ErrJobNotFound, id)}
	}
	return t.mutateLocked(j, kind)
}

// mutateByPrefix applies kind to the one job in (plat, chatID) whose ID starts
// with idPrefix.
func (t *jobTable) mutateByPrefix(idPrefix, plat, chatID string, kind mutationKind) mutationResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, err := t.findByPrefixLocked(idPrefix, plat, chatID)
	if err != nil {
		return mutationResult{lookupErr: err}
	}
	return t.mutateLocked(j, kind)
}

// mutateLocked applies kind to j and marshals the resulting job set in the same
// lock hold, so no other writer can persist a state this mutation later undoes
// (#1272).
func (t *jobTable) mutateLocked(j *Job, kind mutationKind) (r mutationResult) {
	prevEntry, prevPaused := j.entryID, j.Paused
	switch kind {
	case mutDelete:
		r.removeEntry = t.deleteLocked(j)
	case mutPause:
		e, err := t.pauseLocked(j)
		if err != nil {
			return mutationResult{opErr: err}
		}
		r.removeEntry = e
	case mutResume:
		p, err := t.resumeLocked(j)
		if err != nil {
			return mutationResult{opErr: err}
		}
		r.plan = &p
	}
	r.snap, r.persistErr = t.persistLocked()
	if r.persistErr != nil && kind != mutDelete {
		j.entryID, j.Paused = prevEntry, prevPaused
		r.removeEntry, r.plan = 0, nil
	}
	r.job = *j
	return r
}
