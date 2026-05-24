// scheduler_persist.go: cron_jobs.json marshal + monotonic-seq atomic save.
//
// Split out of scheduler.go to keep the lifecycle / job-CRUD / execute paths
// readable; no behaviour change. Callers still invoke s.persistJobsLocked()
// and s.saveMarshaledSeq() exactly as before — these methods stay on
// *Scheduler so the s.mu / s.storeMu / s.saveSeq / s.lastSavedSeq /
// s.storeDirOnce / s.storePath fields remain accessible without exporting.

package cron

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"

	"github.com/naozhi/naozhi/internal/osutil"
)

// marshalJobsFn is the signature of the JSON serializer used by
// marshalJobsLocked. It is swapped via atomic.Pointer in tests (see
// withFailingMarshal) to exercise persist-failure paths without constructing
// a cyclic graph in Job. Kept behind an atomic.Pointer because other cron
// tests in the same package run with t.Parallel(); a naked var swap races
// with concurrent marshalJobsLocked readers under -race.
type marshalJobsFn func(any) ([]byte, error)

var marshalJobs atomic.Pointer[marshalJobsFn]

func init() {
	fn := marshalJobsFn(json.Marshal)
	marshalJobs.Store(&fn)
}

// marshalJobsLocked serialises the current jobs map to JSON while the caller
// still holds s.mu. Round 47: replaces the map clone on every mutation. Safe
// because json.Marshal only reads Job fields (no mutation) and the output []byte
// is independent of s.jobs lifetime, so the caller can drop s.mu immediately.
// The (*Job).entryID field is unexported and therefore invisible to Marshal,
// so the runtime-only value never leaks into cron_jobs.json.
func (s *Scheduler) marshalJobsLocked() ([]byte, error) {
	entries := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		entries = append(entries, j)
	}
	// Sort by ID for deterministic on-disk order. Map iteration is random, so
	// identical in-memory state would produce diff-noisy JSON across saves —
	// breaking git audit of backed-up cron_jobs.json and making post-incident
	// diffs much harder to read.
	//
	// O(N log N) sort 每 mutation 一次；50 jobs × log50 ≈ 280 比较，热路径可接受。
	// NEEDS-DESIGN R241-PERF-9：未来若 jobs 上千，可改增量维护已排 ID slice。
	slices.SortFunc(entries, func(a, b *Job) int { return cmp.Compare(a.ID, b.ID) })
	return (*marshalJobs.Load())(entries)
}

// persistJobsLocked marshals under the caller's s.mu and writes asynchronously.
// Callers hold s.mu (write or read), invoke this to produce the byte payload
// and the save func, unlock, then call the save func. This keeps marshal
// latency in the critical section (needed for snapshot consistency) but moves
// disk I/O + storeMu contention outside.
//
// Return contract:
//   - On success, returns a non-nil save func and nil err. Caller must unlock
//     s.mu before invoking save() so disk I/O does not block the mutex.
//   - On marshal failure, returns (nil, ErrPersistFailed). Caller MUST plumb
//     the error back to the HTTP layer (e.g. map to 500) because the in-memory
//     mutation has already happened and is now unpersisted — a restart would
//     replay the prior on-disk state. marshal failure is only observable under
//     OOM or a broken Job schema; either way an alert-worthy event.
//
// R51-QUAL-001: previously this returned a no-op func on marshal failure,
// so every mutation appeared to succeed even when nothing reached disk.
func (s *Scheduler) persistJobsLocked() (func(), error) {
	data, err := s.marshalJobsLocked()
	if err != nil {
		slog.Error("marshal cron store", "err", err)
		return nil, fmt.Errorf("%w: %w", ErrPersistFailed, err)
	}
	// Capture a monotonic sequence number under s.mu so it totals-orders all
	// marshals with the snapshot state they represent. saveMarshaled skips
	// writes whose seq is older than what has already landed on disk —
	// closes R48-REL-PERSIST-ORDERING-RACE (Go sync.Mutex is not FIFO so a
	// later marshal can reach storeMu before an earlier one).
	seq := s.saveSeq.Add(1)
	return func() { s.saveMarshaledSeq(data, seq) }, nil
}

// saveMarshaledSeq is the mutation-path persist function. It skips the write
// if lastSavedSeq has already advanced past our seq — this happens when Go's
// sync.Mutex hands storeMu to a later writer (larger seq) before us, so our
// data is strictly stale and writing it would roll back the disk state.
// Note: lastSavedSeq is read+stored under storeMu (Load+Store pattern), not a
// CAS — storeMu serialises both the staleness check and the disk write so a
// later seq can never race past us between Load and Store. Closes R48-REL-
// PERSIST-ORDERING-RACE. R232-CR-11.
func (s *Scheduler) saveMarshaledSeq(data []byte, seq uint64) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	if s.storePath == "" {
		return
	}
	if last := s.lastSavedSeq.Load(); seq <= last {
		// A newer snapshot already won the storeMu race. Dropping our write
		// is safe — the newer payload already contains every field we would
		// have persisted (mutations under s.mu are linearised by s.mu, so
		// seq order matches state order).
		slog.Debug("cron save skipped: newer snapshot already saved",
			"our_seq", seq, "last_saved_seq", last)
		return
	}
	// R235-SEC-6: parent dir 0700 mirrors runStore.newRunStore (R234-SEC-4).
	// cron_jobs.json itself is mode 0600 (operator prompts + chat IDs), but
	// without an explicit parent-dir clamp the file's existence and name leak
	// to other local users via the default XDG config dir mode (often 0755).
	// sync.Once keeps the MkdirAll out of the per-mutation hot path; if the
	// directory disappears later (operator rm -rf), WriteFileAtomic will
	// surface ENOENT and the operator can recover by restarting.
	s.storeDirOnce.Do(func() {
		if dir := filepath.Dir(s.storePath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				slog.Warn("cron store parent dir mkdir failed", "err", err, "dir", dir)
			}
		}
	})
	if err := osutil.WriteFileAtomic(s.storePath, data, 0600); err != nil {
		slog.Error("save cron store", "err", err, "disk_full", osutil.IsDiskFull(err))
		return
	}
	s.lastSavedSeq.Store(seq)
}
