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

// marshalEntriesPool reuses the []*Job snapshot slice that
// marshalJobsLocked builds + sorts on every mutation. With ~10-50 jobs in
// steady-state, the slice is the dominant transient alloc on the
// finishRun → persist → save hot path (dashboard 1Hz mutations + every
// cron tick's recordTerminalResult call land here). Pooling the slice
// drops the per-call cap-len(jobs) backing-array allocation; the JSON
// payload still allocates fresh because saveMarshaledSeq holds the bytes
// across the async storeMu write so we can't reuse them.
//
// Slot capacity convention: pool returns a fresh slice when len(s.jobs)
// exceeds the cap of the pooled slice; otherwise we reslice down to len
// 0 and append. On Put we drop slices whose cap exceeds 4×maxJobsHardCap
// (= 2000) so a one-time burst that grew the slice past steady-state
// can't pin a multi-MB backing array forever. R247-PERF-11 (#551).
var marshalEntriesPool = sync.Pool{
	New: func() any {
		// Default seed sized for the common 50-job case. Larger schedulers
		// grow the slice on first use (regular append doubling) and the
		// grown slice is what circulates through Put/Get afterwards.
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
		// Drop oversize slices instead of recycling so a one-off burst
		// can't pin its inflated backing array via the pool. Leaving
		// it for GC is cheaper than a sync.Pool with an unbounded high
		// watermark.
		return
	}
	// Zero out element pointers before return so the pool doesn't pin
	// dead *Job pointers via stale slice slots (otherwise a deleted job
	// would stay reachable until the next persist call overwrites the
	// slot). Reset length to 0; cap is preserved so the next Get can
	// append without realloc up to that cap.
	for i := range *s {
		(*s)[i] = nil
	}
	*s = (*s)[:0]
	marshalEntriesPool.Put(s)
}

// jobIDCmpForSort is the package-level comparator slices.SortFunc uses inside
// marshalJobsLocked. Hoisted out of the call site (was an inline closure
// literal) so the per-mutation persist hot path does not allocate the
// closure header on every invocation. R20260527122801-PERF-2 (#1340) partial:
// the broader proposal moves the entire sort/marshal out of the s.mu Write
// critical section by value-copying *Job into entries; that requires a
// deeper refactor of the rollback contract (callers expect marshal errors
// synchronously to roll back in-memory mutation), so this commit lands
// the alloc-trim half — reduces persistJobsLocked allocs/op without
// touching the lock-vs-marshal-error invariant.
func jobIDCmpForSort(a, b *Job) int {
	return cmp.Compare(a.ID, b.ID)
}

// marshalJobsFn is the signature of the JSON serializer used by
// marshalJobsLocked. It is swapped via the per-Scheduler atomic.Pointer in
// tests (see withFailingMarshal) to exercise persist-failure paths without
// constructing a cyclic graph in Job. Kept behind an atomic.Pointer because
// other cron tests in the same package run with t.Parallel(); a naked field
// swap races with concurrent marshalJobsLocked readers under -race.
//
// R250-ARCH-14: lifted from a package-level var to a *Scheduler field so a
// failing-marshal test in one parallel run cannot leak into another scheduler
// instance, and so the test seam no longer pokes a hole through prod surface.
//
// R250-ARCH-14 closes the cluster of older anchors that all flagged the same
// package-level-mutable shape:
//   - R242-CR-5 (#693): "atomic.Pointer pkg-level mutable via init() — DI field"
//   - R246-ARCH-18 / R247-CR-19 (#599): "test seam loaded via init()"
//
// The current shape stores defaultMarshalJobs as a package-level var that is
// read-only after init (so we can take its address without per-Scheduler
// allocation) and the *Scheduler initialiser does Store(&defaultMarshalJobs).
// Per-Scheduler atomic.Pointer.Swap is what tests use; no init() seam, no
// global mutation. Pinned by TestMarshalJobs_PerSchedulerIsolation.
type marshalJobsFn func(any) ([]byte, error)

// defaultMarshalJobs is the production serializer plumbed into every
// *Scheduler.marshalJobs slot at NewScheduler. Stored as a package var
// (read-only after init) so the *Scheduler initialiser can take its
// address without allocating a fresh closure per scheduler.
var defaultMarshalJobs = marshalJobsFn(json.Marshal)

// marshalJobsLocked serialises the current jobs map to JSON while the caller
// still holds s.mu. Round 47: replaces the map clone on every mutation. Safe
// because json.Marshal only reads Job fields (no mutation) and the output []byte
// is independent of s.jobs lifetime, so the caller can drop s.mu immediately.
// The (*Job).entryID field is unexported and therefore invisible to Marshal,
// so the runtime-only value never leaks into cron_jobs.json.
//
// R247-PERF-11 (#551): the entries snapshot slice is pulled from
// marshalEntriesPool so the per-mutation cap-len(jobs) backing-array
// allocation amortises across calls. The output []byte is still freshly
// allocated each call because saveMarshaledSeq holds it across the
// asynchronous storeMu write — pooling those bytes would race the
// concurrent reader in saveMarshaledSeq.
func (s *Scheduler) marshalJobsLocked() ([]byte, error) {
	entriesPtr := marshalEntriesPool.Get().(*[]*Job)
	defer putMarshalEntries(entriesPtr)
	entries := *entriesPtr
	// Grow when the pooled slice's cap is below current job count. The pool
	// circulates the grown slice so steady-state hits skip this branch.
	if cap(entries) < len(s.jobs) {
		entries = make([]*Job, 0, len(s.jobs))
	}
	// R164029-PERF-9 (#1598): emit in s.sortedJobIDs order so the on-disk
	// JSON stays deterministic WITHOUT running an O(N log N) slices.SortFunc
	// inside the s.mu critical section on every mutation. The sorted-ID slice
	// is maintained incrementally at the two s.jobs mutation seams
	// (addToChatIndexLocked / deleteJobLocked) via binary-search insert/delete.
	//
	// s.jobs remains the source of truth: the slice is only a hint. We
	// validate it still matches the map (same length, every ID resolvable)
	// while appending; if it drifted — which only happens when a test helper
	// pokes s.jobs[id] directly without the seam — we fall back to building
	// + sorting from the map so a job is never silently dropped from the
	// snapshot. The fallback also covers the empty/single-job case the prior
	// R241-PERF-9 (#482) fast path special-cased: iterating 0/1 IDs needs no
	// sort either way.
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
		// Drift fallback: rebuild from the map (authoritative) and sort.
		// R20260527122801-PERF-2 (#1340): package-level comparator avoids a
		// per-call closure-header allocation; behaviour is identical
		// (cmp.Compare on Job.ID). This path is cold in production — only
		// direct-poke test helpers reach it.
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
		// Defensive fallback: a *Scheduler constructed via the zero
		// value (or a future code path that forgets to initialise the
		// field) still uses the production marshaller rather than
		// nil-deref panicking the persist hot path.
		return defaultMarshalJobs(entries)
	}
	return (*fn)(entries)
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
//
// R244-GO-P3-1: the marshal-failure return uses Go 1.20+ multi-%w
// (`fmt.Errorf("%w: %w", ErrPersistFailed, err)`) so callers can
// `errors.Is(retErr, ErrPersistFailed)` (sentinel match — preferred for
// HTTP 500 mapping) AND `errors.Is(retErr, &json.UnsupportedTypeError{})`
// or other underlying-cause sentinel match in the same chain. Equivalently
// `errors.As` walks both wrapped errors and binds the first matching
// target; ordering puts ErrPersistFailed first so a generic "is the
// mutation persisted?" check short-circuits before walking into the
// json/encoding error chain. See std `errors` package docs §"Wrapping
// multiple errors".
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

// jobsSnapshot is a marshal-ready capture of the persisted job set taken under
// s.mu. The entries are value copies of *Job in sortedJobIDs order, fully
// detached from s.jobs so json.Marshal can run AFTER the caller drops s.mu
// (R20260607-PERF-005 / #1923) without racing concurrent mutators. The seq is
// taken under the same critical section so saveMarshaledSeq's staleness gate
// still total-orders this payload against every other persist (seq order ==
// state order; s.mu linearises both the field mutation and this seq grab).
type jobsSnapshot struct {
	entries []*Job
	seq     uint64
}

// snapshotJobsForSaveLocked captures the current job set into a detached
// marshal-ready snapshot under the caller's s.mu, plus the persist seq.
//
// Unlike marshalJobsLocked (which marshals live *Job pointers inside the
// critical section, valid because the lock is held throughout), the returned
// entries are value copies so the caller can release s.mu and marshal them
// off the hot lock. Job is a flat value type — the only pointer field is
// Notify *bool, which mutators replace wholesale (j.Notify = &v) rather than
// dereference-write, so the copied pointer keeps addressing an immutable bool
// and json.Marshal can read it after unlock without a data race. entryID /
// cachedPeriod / cachedSched are runtime-only and excluded from JSON anyway.
//
// The sorted-ID hint and drift fallback mirror marshalJobsLocked so the
// on-disk ordering stays byte-identical regardless of which persist path ran.
func (s *Scheduler) snapshotJobsForSaveLocked() jobsSnapshot {
	entries := make([]*Job, 0, len(s.jobs))
	useHint := len(s.sortedJobIDs) == len(s.jobs)
	if useHint {
		for _, id := range s.sortedJobIDs {
			j, ok := s.jobs[id]
			if !ok {
				useHint = false
				break
			}
			cp := *j
			// R20260608133928-CR-5: deep-copy Notify (*bool) to avoid alias with
			// live job during off-lock json.Marshal (R20260607-PERF-005 / #1923).
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
			// R20260608133928-CR-5: deep-copy Notify (*bool) to avoid alias with
			// live job during off-lock json.Marshal (R20260607-PERF-005 / #1923).
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
	return jobsSnapshot{entries: entries, seq: s.saveSeq.Add(1)}
}

// persistSnapshot marshals a detached snapshot taken by
// snapshotJobsForSaveLocked and returns the save func. It does NOT require
// s.mu — the whole point (R20260607-PERF-005 / #1923) is to keep json.Marshal
// out of the write critical section. On marshal failure it returns
// (nil, ErrPersistFailed) so the caller can roll back the in-memory mutation,
// preserving persistJobsLocked's rollback contract. The seq captured under
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
// if lastSavedSeq has already advanced past our seq — this happens when Go's
// sync.Mutex hands storeMu to a later writer (larger seq) before us, so our
// data is strictly stale and writing it would roll back the disk state.
// Note: lastSavedSeq is read+stored under storeMu (Load+Store pattern), not a
// CAS — storeMu serialises both the staleness check and the disk write so a
// later seq can never race past us between Load and Store. Closes R48-REL-
// PERSIST-ORDERING-RACE. R232-CR-11.
//
// Failure semantics (R247-GO-15): the WriteFileAtomic error path returns
// WITHOUT bumping lastSavedSeq. This is intentional and load-bearing for
// the staleness gate above:
//
//   - A failed write means our payload (carrying state up to seq N) never
//     reached disk. If we still bumped lastSavedSeq=N, a strictly later
//     mutation that already raced into storeMu with seq M (M > N) and is
//     now waiting on s.storeMu would be the next one to persist — fine in
//     isolation — BUT if that later writer ALSO fails, the gate would
//     reject any retry path of seq N+1..M-1 even though disk is now
//     stale relative to in-memory state. Keeping lastSavedSeq pinned at
//     the last *successful* write means a subsequent Append-driven save
//     (or Stop-time save) carrying any seq > lastSavedSeq is always
//     allowed to attempt a fresh write.
//
//   - The MkdirAll branch logs and falls through (does NOT return), so a
//     transient directory error still gets a WriteFileAtomic attempt —
//     that's the only "fail-but-bump" path we'd accept and it doesn't
//     reach the lastSavedSeq.Store line either way.
//
// Disk-full is a special case worth calling out: osutil.IsDiskFull(err)
// is logged so operators can correlate with cron persist gaps; once the
// disk recovers the next mutation's save will land naturally because the
// gate is still pinned to the pre-disk-full seq.
//
// FSYNC-COST-PROFILE (R20260527122801-PERF-1 / #1333): each mutation lands
// here synchronously, so finishRun + AddJob + UpdateJob each pay one
// WriteFileAtomic = ~4 syscalls + 2× fsync (data + dir) per call. On NFS /
// EBS / slow SSD the per-call latency reaches seconds and serialises every
// concurrent mutator on storeMu. The staleness gate above lets multiple
// mutations coalesce naturally only when storeMu is contended; the
// proposed full fix (200ms storeMu-batched debounce + once-only SyncDir)
// requires changing the contract above so callers do not assume "save()
// returned ⇒ on disk before next mutation reads cron_jobs.json". Tracked
// as needs-design under #1333; until then operators on slow disks should
// pin maxJobs lower so the per-mutation fsync × N traffic stays bounded.
//
// RES2 (#400) is the parity-with-session.saveIfDirty framing of the same
// deferral: adopting a single saveLoop goroutine + dirty flag would trade
// the synchronous "save() returned ⇒ on disk" determinism (which the
// rollback contract in persistJobsLocked's godoc relies on) for amortised
// async writes. Won't-fix as a standalone change because R58 already
// hoisted WriteFileAtomic out of s.mu (the save-closure pattern) so cron
// is no longer hot enough to justify losing the determinism; the async
// loop is gated on the same contract change as #1333 above, not adopted
// independently. Kept issue-backed so the parity ask has a tracker.
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
	//
	// R238-SEC-10 (#830): MkdirAll skips perm changes on an existing dir;
	// follow with Chmod(0o700) so a pre-existing 0o755 parent gets clamped.
	// Mirror of the eager NewScheduler path.
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
		// R247-GO-15: do NOT Store(seq) on failure — see godoc above.
		// Pinning lastSavedSeq at the last successful write keeps the
		// staleness gate permissive enough that a retry from any later
		// mutation can still attempt to land a fresh snapshot.
		slog.Error("save cron store", "err", err, "disk_full", osutil.IsDiskFull(err))
		// R247-GO-15: deliberately do NOT advance lastSavedSeq on write
		// failure. The seq counter tracks "what is on disk", not "what we
		// most recently tried to persist" — bumping it here would let a
		// subsequent in-flight save with seq < this one's seq incorrectly
		// trip the staleness short-circuit above and skip its (presumably
		// successful) write, causing the disk state to lag the in-memory
		// state across the next read until another mutation comes along.
		// Leaving lastSavedSeq pinned to the last *successful* write means
		// the next mutator's saveMarshaledSeq will see (seq > last) and
		// retry persistence with the freshest snapshot, so a transient
		// ENOSPC / EIO recovers without operator intervention.
		return
	}
	s.lastSavedSeq.Store(seq)
}
