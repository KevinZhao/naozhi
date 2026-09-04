package cron

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/naozhi/naozhi/internal/osutil"
)

// marshalEntriesPool reuses the []*Job snapshot slice that marshalJobsLocked
// builds on every mutation — the dominant transient alloc on the finishRun →
// persist hot path. The JSON payload still allocates fresh because
// saveMarshaledSeq holds the bytes across the async storeMu write. Put drops
// slices whose cap exceeds marshalEntriesCapDrop so a burst cannot pin a
// large backing array forever (#551).
var marshalEntriesPool = sync.Pool{
	New: func() any {
		// Sized for the common 50-job case; larger schedulers grow on first use.
		s := make([]*Job, 0, 64)
		return &s
	},
}

// marshalEntriesCapDrop is the cap threshold above which Put refuses
// to recycle a slice. Keeps the pool's working set bounded even after
// a transient burst inflated one slot far past steady-state.
const marshalEntriesCapDrop = 4 * maxJobsHardCap // 2000 *Job slots

// putMarshalEntries returns the slice to the pool. Nil-checked so the
// fallback path in marshalJobsLocked (where a brand-new slice was used
// because the pool was empty) stays safe.
func putMarshalEntries(s *[]*Job) {
	if s == nil {
		return
	}
	if cap(*s) > marshalEntriesCapDrop {
		// Drop oversize slices rather than pin the inflated backing array via the pool.
		return
	}
	// Zero element pointers so the pool doesn't keep deleted *Job values
	// reachable; keep cap so the next Get appends without realloc.
	for i := range *s {
		(*s)[i] = nil
	}
	*s = (*s)[:0]
	marshalEntriesPool.Put(s)
}

// jobIDCmpForSort is the package-level comparator for marshalJobsLocked's
// drift fallback, hoisted so the persist hot path does not allocate a closure
// header per call (#1340).
func jobIDCmpForSort(a, b *Job) int {
	return cmp.Compare(a.ID, b.ID)
}

// marshalJobsFn is the signature of the JSON serializer used by
// marshalJobsLocked. Tests swap it via the per-Scheduler atomic.Pointer
// (see withFailingMarshal) to exercise persist-failure paths; atomic because
// parallel tests would otherwise race concurrent marshalJobsLocked readers,
// per-Scheduler so one test's failing marshal cannot leak into another
// instance. Pinned by TestMarshalJobs_PerSchedulerIsolation.
type marshalJobsFn func(any) ([]byte, error)

// defaultMarshalJobs is the production serializer plumbed into every
// *Scheduler.marshalJobs slot at NewScheduler. Stored as a package var
// (read-only after init) so the *Scheduler initialiser can take its
// address without allocating a fresh closure per scheduler.
var defaultMarshalJobs = marshalJobsFn(json.Marshal)

// marshalJobsLocked serialises the current jobs map to JSON while the caller
// still holds s.mu. Safe because json.Marshal only reads Job fields and the
// output []byte is independent of s.jobs, so the caller can drop s.mu
// immediately. The unexported entryID never leaks into cron_jobs.json.
// The entries slice comes from marshalEntriesPool; the output bytes are fresh
// each call because saveMarshaledSeq holds them across the async storeMu write.
func (s *Scheduler) marshalJobsLocked() ([]byte, error) {
	entriesPtr := marshalEntriesPool.Get().(*[]*Job)
	defer putMarshalEntries(entriesPtr)
	entries := *entriesPtr
	// Grow when the pooled cap is below the job count; the grown slice circulates back.
	if cap(entries) < len(s.jobs) {
		entries = make([]*Job, 0, len(s.jobs))
	}
	// Emit in s.sortedJobIDs order so on-disk JSON stays deterministic without an
	// O(N log N) sort inside the s.mu critical section (#1598). s.jobs remains the
	// source of truth: if the hint drifted (a test helper poking s.jobs directly)
	// fall back to building + sorting from the map so no job is silently dropped.
	useHint := len(s.sortedJobIDs) == len(s.jobs)
	if useHint {
		for _, id := range s.sortedJobIDs {
			j, ok := s.jobs[id]
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
		for _, j := range s.jobs {
			entries = append(entries, j)
		}
		if len(entries) > 1 {
			slices.SortFunc(entries, jobIDCmpForSort)
		}
	}
	*entriesPtr = entries
	fn := s.marshalJobs.Load()
	if fn == nil {
		// Zero-value *Scheduler (or a path that forgot the field) still uses the
		// production marshaller.
		return defaultMarshalJobs(entries)
	}
	return (*fn)(entries)
}

// persistJobsLocked marshals under the caller's s.mu and writes asynchronously:
// the caller produces the payload + save func here, unlocks, then calls save().
// Marshal latency stays in the critical section (snapshot consistency); disk
// I/O + storeMu contention move outside.
//
// On success returns a non-nil save func; caller must unlock s.mu before
// invoking it. On marshal failure returns (nil, ErrPersistFailed) wrapped with
// the cause via multi-%w so callers can errors.Is either. The caller MUST
// surface it (e.g. HTTP 500): the in-memory mutation already happened and is
// now unpersisted, so a restart would replay the prior on-disk state.
func (s *Scheduler) persistJobsLocked() (func(), error) {
	data, err := s.marshalJobsLocked()
	if err != nil {
		slog.Error("marshal cron store", "err", err)
		return nil, fmt.Errorf("%w: %w", ErrPersistFailed, err)
	}
	// Monotonic seq captured under s.mu total-orders marshals with the state they
	// represent; saveMarshaledSeq skips writes older than what already landed
	// (sync.Mutex is not FIFO, so a later marshal can reach storeMu first).
	seq := s.saveSeq.Add(1)
	return func() { s.saveMarshaledSeq(data, seq) }, nil
}

// jobsSnapshot is a marshal-ready capture of the persisted job set taken under
// s.mu: value copies of *Job in sortedJobIDs order, detached from s.jobs so
// json.Marshal can run AFTER the caller drops s.mu (#1923). seq is taken in the
// same critical section so saveMarshaledSeq's staleness gate still applies.
type jobsSnapshot struct {
	entries []*Job
	seq     uint64
	// pooled, when non-nil, is the marshalEntriesPool handle backing entries;
	// persistSnapshot returns it via putMarshalEntries after marshal (#1975).
	pooled *[]*Job
}

// snapshotJobsForSaveLocked captures the current job set into a detached
// marshal-ready snapshot under the caller's s.mu, plus the persist seq.
// Entries are value copies so the caller can release s.mu and marshal off the
// hot lock. Job is a flat value type — the only pointer field is Notify *bool,
// which is deep-copied so the off-lock marshal cannot race a mutator; entryID /
// cachedPeriod / cachedSched are runtime-only and excluded from JSON. The
// sorted-ID hint and drift fallback mirror marshalJobsLocked so the on-disk
// ordering is byte-identical regardless of which persist path ran.
func (s *Scheduler) snapshotJobsForSaveLocked() jobsSnapshot {
	// Pooled outer slice (same pool as marshalJobsLocked), returned by
	// persistSnapshot after marshal (#1975).
	entriesPtr := marshalEntriesPool.Get().(*[]*Job)
	entries := *entriesPtr
	if cap(entries) < len(s.jobs) {
		entries = make([]*Job, 0, len(s.jobs))
	} else {
		entries = entries[:0]
	}
	useHint := len(s.sortedJobIDs) == len(s.jobs)
	if useHint {
		for _, id := range s.sortedJobIDs {
			j, ok := s.jobs[id]
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
		for _, j := range s.jobs {
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
	return jobsSnapshot{entries: entries, seq: s.saveSeq.Add(1), pooled: entriesPtr}
}

// persistSnapshot marshals a detached snapshot taken by
// snapshotJobsForSaveLocked and returns the save func without holding s.mu
// (#1923). On marshal failure returns (nil, ErrPersistFailed) so the caller can
// roll back, preserving persistJobsLocked's contract; the seq captured under
// the lock is reused so saveMarshaledSeq's ordering gate is unaffected.
func (s *Scheduler) persistSnapshot(snap jobsSnapshot) (func(), error) {
	var data []byte
	var err error
	if fn := s.marshalJobs.Load(); fn != nil {
		data, err = (*fn)(snap.entries)
	} else {
		data, err = defaultMarshalJobs(snap.entries)
	}
	// data is independent of snap.entries now, so return the pooled slice on both
	// success and failure paths.
	putMarshalEntries(snap.pooled)
	if err != nil {
		slog.Error("marshal cron store", "err", err)
		return nil, fmt.Errorf("%w: %w", ErrPersistFailed, err)
	}
	seq := snap.seq
	return func() { s.saveMarshaledSeq(data, seq) }, nil
}

// saveMarshaledSeq is the mutation-path persist function. It skips the write
// when lastSavedSeq has already advanced past seq: storeMu handed a later
// writer through first, so our payload is strictly stale. lastSavedSeq is
// Load+Store under storeMu (no CAS — storeMu serialises check + write).
//
// On WriteFileAtomic failure lastSavedSeq is deliberately NOT bumped: it tracks
// "what is on disk", so pinning it at the last successful write keeps the gate
// permissive and the next mutation retries with a fresh snapshot. Each mutation
// pays one WriteFileAtomic (~4 syscalls + 2 fsyncs); debouncing would break the
// "save() returned ⇒ on disk" contract callers rely on (see #1333).
func (s *Scheduler) saveMarshaledSeq(data []byte, seq uint64) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	if s.storePath == "" {
		return
	}
	if last := s.lastSavedSeq.Load(); seq <= last {
		// A newer snapshot already won the storeMu race; it contains every field we
		// would have persisted.
		slog.Debug("cron save skipped: newer snapshot already saved",
			"our_seq", seq, "last_saved_seq", last)
		return
	}
	// Parent dir clamped to 0700 (mirrors newRunStore): cron_jobs.json is 0600 but
	// a default 0755 config dir leaks the file's existence to other local users.
	// sync.Once keeps MkdirAll off the per-mutation hot path; Chmod follows because
	// MkdirAll skips perm changes on an existing dir (#830).
	s.storeDirOnce.Do(func() {
		if dir := filepath.Dir(s.storePath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				slog.Warn("cron store parent dir mkdir failed", "err", err, "dir", dir)
			}
			if err := os.Chmod(dir, 0o700); err != nil && !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("cron store parent dir chmod failed", "err", err, "dir", dir)
			}
		}
	})
	if err := osutil.WriteFileAtomic(s.storePath, data, 0600); err != nil {
		// Do NOT advance lastSavedSeq on failure — see godoc.
		slog.Error("save cron store", "err", err, "disk_full", osutil.IsDiskFull(err))
		return
	}
	s.lastSavedSeq.Store(seq)
}
