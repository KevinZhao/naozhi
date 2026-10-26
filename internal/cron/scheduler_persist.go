package cron

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
)

// jobIDCmpForSort is the package-level comparator for marshalLocked's
// drift fallback, hoisted so the persist hot path does not allocate a closure
// header per call (#1340).
func jobIDCmpForSort(a, b *Job) int {
	return cmp.Compare(a.ID, b.ID)
}

// marshalJobsFn is the signature of the JSON serializer used by
// marshalLocked. Tests swap it via the per-Scheduler atomic.Pointer
// (see withFailingMarshal) to exercise persist-failure paths; atomic because
// parallel tests would otherwise race concurrent marshalLocked readers,
// per-Scheduler so one test's failing marshal cannot leak into another
// instance.
type marshalJobsFn func(any) ([]byte, error)

// defaultMarshalJobs is the production serializer plumbed into every
// *Scheduler.marshalJobs slot at NewScheduler. Stored as a package var
// (read-only after init) so the *Scheduler initialiser can take its
// address without allocating a fresh closure per scheduler.
var defaultMarshalJobs = marshalJobsFn(json.Marshal)

// save writes a snapshot the table marshaled. Caller must not hold s.tbl.mu.
func (s *Scheduler) save(m marshaledJobs) { s.saveMarshaledSeq(m.data, m.seq) }

// jobsSnapshot is a marshal-ready capture of the persisted job set taken under
// s.tbl.mu: value copies of *Job in sortedJobIDs order, detached from s.tbl.jobs so
// json.Marshal can run AFTER the caller drops s.tbl.mu (#1923). seq is taken in the
// same critical section so saveMarshaledSeq's staleness gate still applies.
type jobsSnapshot struct {
	entries []*Job
	seq     uint64
}

// persistSnapshot marshals a detached snapshot taken by
// snapshotForSaveLocked and returns the save func without holding s.tbl.mu
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
	// Parent dir clamped to 0700 (mirrors the run store): cron_jobs.json is 0600 but
	// a default 0755 config dir leaks the file's existence to other local users.
	// sync.Once keeps MkdirAll off the per-mutation hot path; Chmod follows because
	// MkdirAll skips perm changes on an existing dir (#830).
	s.storeDirOnce.Do(func() {
		if dir := datadir.ForStore(s.storePath).Root(); dir != "" && dir != "." {
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
