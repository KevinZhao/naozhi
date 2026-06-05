// scheduler_jobs.go: cron Job CRUD path.
//
// Contains every public mutation API (AddJob / DeleteJob / PauseJob /
// ResumeJob / UpdateJob / SetJobPrompt / TriggerNow), every list / lookup
// API, schedule-preview helpers, and the robfig-cron entry registration
// (registerJob) that hooks each Job to the scheduler. Split out of
// scheduler.go to give CRUD its own file separate from the run-time hot
// path (scheduler_run.go) and the lifecycle bootstrap (scheduler.go).
//
// No behaviour change. Methods stay on *Scheduler so private fields
// remain accessible without exporting.

package cron

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	robfigcron "github.com/robfig/cron/v3"
)

// listEntryIDsPool reuses the transient []cronEntryID scratch slice
// recorded during ListAllJobsWithNextRun's two-phase snapshot. Dashboard
// polls at 1Hz across multiple tabs so the call frequency × jobs-per-call
// dominates the cron CRUD path's allocator pressure. Get returns a
// zero-length slice with potentially non-zero capacity — callers must
// reset length (`:0`) when they Put it back. R247-PERF-4 (#530).
//
// R250-PERF-15 (#1118): previously this pooled a []listSnapshotPair whose
// element embedded a full Job value-copy (~300B incl. RunCounters), so the
// snapshot copied every Job twice — once into the pooled scratch under
// RLock, then again into the caller-owned result slice. With N jobs × N
// tabs × 1Hz that wasted ~750KB/s of GC churn on the redundant copy. We
// now copy each Job exactly once (directly into result under RLock) and the
// pooled scratch holds only the cheap entryID (8 bytes) needed to patch
// NextRun after s.cron.Entries() runs lock-free.
var listEntryIDsPool = sync.Pool{
	New: func() any {
		s := make([]cronEntryID, 0, 64)
		return &s
	},
}

// listNextByIDPool reuses the EntryID -> Next time map. Reset via
// `clear()` (Go 1.21) before re-Put so stale keys from a larger previous
// snapshot don't leak into a smaller current one. R247-PERF-4 (#530).
var listNextByIDPool = sync.Pool{
	New: func() any {
		m := make(map[cronEntryID]time.Time, 64)
		return &m
	},
}

// AddJob validates, registers, and persists a new cron job.
func (s *Scheduler) AddJob(j *Job) error {
	if err := validateSchedule(j.Schedule, s.previewLocation()); err != nil {
		return fmt.Errorf("invalid schedule %q: %w", j.Schedule, err)
	}
	// Title 长度校验在 scheduler 层兜底，避免绕过 dashboard handler（例如
	// store 直接加载被篡改的 cron_jobs.json）把超长字符串持久化进内存。
	if n := utf8.RuneCountInString(j.Title); n > MaxCronTitleLen {
		return fmt.Errorf("title too long: %d runes > %d cap", n, MaxCronTitleLen)
	}
	// R244-SEC-P2-5 / #889: AddJob is the canonical create path; mirror
	// SetJobPrompt's strict prompt validation so any non-dashboard caller
	// (test, IM op, future API) cannot persist multi-MB / log-injection
	// prompts to cron_jobs.json. Empty prompts are permitted because the
	// dashboard creates jobs in a paused-with-empty-prompt state to be
	// filled in via SetJobPrompt later.
	if j.Prompt != "" {
		if err := ValidatePromptStrict(j.Prompt); err != nil {
			return err
		}
	}
	// R250-CR-8 (#1141): defence-in-depth — IM dispatch and dashboard
	// handlers validate WorkDir/Backend/Notify* before calling AddJob,
	// but a future internal caller reaching AddJob directly would
	// otherwise persist arbitrary bytes. The same caps loadJobs already
	// applies on the read path now run on the write path too, so no
	// hand-crafted in-memory job reaches cron_jobs.json with a multi-KB
	// WorkDir or log-injection bytes in NotifyChatID.
	if err := validateJobFields(j); err != nil {
		return err
	}

	// addJobAcquiringLock runs under s.mu (defer Unlock). Splitting the locked
	// section into a helper means every early-return path goes through
	// defer and removes the prior pattern of 4 manual s.mu.Unlock() calls
	// (R228-GO-2): adding a new validation step inside the locked section
	// no longer risks leaking a held mutex on the new error path.
	save, stub, rollbackEntryID, perr := s.addJobAcquiringLock(j)
	if perr != nil {
		// R20260605B-CORR-6 (#1810): on the persist-failure rollback path
		// addJobAcquiringLock zeroed the cron entry under s.mu but deferred
		// the actual s.cron.Remove to here so it does not block the unbuffered
		// c.remove channel send while the write lock is held. rollbackEntryID
		// is 0 on every other error path (capacity rejection — nothing was
		// registered with cron), and Remove(0) is a no-op, so this is safe to
		// call unconditionally in the error branch.
		if rollbackEntryID != 0 {
			s.cron.Remove(rollbackEntryID)
		}
		// addJobAcquiringLock may surface either a pre-mutation error (capacity
		// rejection — no save returned) or a post-mutation persist error
		// (in-memory insertion already happened). The caller cannot tell
		// the two apart from the error alone, but in either case there
		// is no save() to invoke — addJobAcquiringLock returns nil for save in
		// both branches.
		return perr
	}
	save()
	// R250-GO-5 (#1068): use the snapshotted fields captured under s.mu
	// instead of re-reading *j after lock release. UpdateJob / SetJobPrompt
	// already follow this pattern (see scheduler.go:1163-1165 R232-CR-12);
	// AddJob was the lone outlier passing the bare *Job pointer to
	// registerStubFromJob, so a concurrent UpdateJob targeting the same id
	// could race the stub-register's reads of WorkDir/Prompt/LastSessionID.
	// The new-ID race is bounded today (no other caller has seen it yet)
	// but the snapshot rule is meant as a uniform invariant — making AddJob
	// comply removes the structural drift hazard a future refactor could
	// turn into a real race.
	s.registerStubByValue(stub.id, stub.workDir, stub.prompt, stub.lastSessionID)
	return nil
}

// addJobStubFields bundles the lock-held snapshot of the fields AddJob
// passes to registerStubByValue. Captured inside addJobAcquiringLock under
// s.mu so a concurrent UpdateJob / SetJobPrompt cannot mutate them between
// addJobAcquiringLock returning and AddJob calling registerStubByValue.
// R250-GO-5 (#1068).
type addJobStubFields struct {
	id            string
	workDir       string
	prompt        string
	lastSessionID string
}

// addJobAcquiringLock performs the AddJob mutation. Unlike the
// pause/resume/deleteJobLocked siblings (caller-holds-lock convention),
// this helper owns the lifecycle of s.mu — it acquires the lock at entry
// and defers Unlock so every early-return path goes through one place.
// Renamed from addJobLocked (R230C-CR-3 / R228-GO-2): the *Locked suffix in
// this package denotes "caller already holds s.mu", which AddJob's helper
// does not satisfy. The new name keeps the contract obvious at the
// call-site.
// The returned rollbackEntryID is non-zero only on the persist-failure
// rollback path: deleteJobLocked snapshots the cron entry under s.mu but the
// actual s.cron.Remove is hoisted to the caller (AddJob) so it runs AFTER
// s.mu is released — see deleteJobLocked / R20260605B-CORR-6 (#1810). It is 0
// on every success and on the pre-mutation capacity-rejection path.
func (s *Scheduler) addJobAcquiringLock(j *Job) (save func(), stub addJobStubFields, rollbackEntryID cronEntryID, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.jobs) >= s.maxJobs {
		return nil, addJobStubFields{}, 0, fmt.Errorf("max cron jobs reached (%d)", s.maxJobs)
	}

	// Per-chat limit to prevent one chat from exhausting global quota.
	// R237-PERF-5 (#661): O(1) lookup on s.chatJobCount replaces the prior
	// O(maxJobs) linear scan. The scan held s.mu across up to 500 *Job
	// entries on every AddJob — a direct hot-path block on the dashboard
	// 1Hz add path that also stalled TriggerNow / emitRunStarted callers
	// contending on the same mutex. The counter is maintained synchronously
	// in deleteJobLocked / Start so it stays in lock-step with len-by-chat
	// (s.jobs); s.jobs is still the canonical truth, asserted by
	// TestChatJobCount_TracksJobsByChat.
	chatKey := chatKeyFor(j.Platform, j.ChatID)
	if s.chatJobCount[chatKey] >= s.maxJobsPerChat {
		return nil, addJobStubFields{}, 0, fmt.Errorf("per-chat cron limit reached (%d)", s.maxJobsPerChat)
	}

	id, err := generateID()
	if err != nil {
		// R242-CR-14 (#706): crypto/rand 失败由 generateID 透传，AddJob 是
		// 公共入口（dashboard / IM 创建任务），失败应表现为 add 请求拒绝
		// 而非进程 panic。10 次重试圈仅对 ID 碰撞有意义；rand 整体失效
		// 时所有重试都会复现同一错误，提早 bail 比循环 10 次更诚实。
		return nil, addJobStubFields{}, 0, fmt.Errorf("cron: generate job id: %w", err)
	}
	j.ID = id
	// Retry on unlikely ID collision. Bound the loop so a hypothetical
	// degenerate generateID (e.g., a test that injects a deterministic mock
	// or a /dev/urandom failure path) cannot spin AddJob under s.mu and
	// stall the whole scheduler. 10 attempts of 8-byte hex IDs is well
	// beyond any realistic collision rate for maxJobsHardCap=500.
	//
	// R247-GO-13 (#493): the original loop emitted Warn on every retry to
	// surface deterministic-generator regressions early. The collision-on-
	// real-rand probability is vanishingly small (~ 500/2^64 per call);
	// emitting 10 Warns is an unambiguous deterministic-generator signal
	// already, but it floods logs with redundant lines. Trim the noise:
	// log Warn once on the first collision (still flags the regression),
	// detect "same ID twice in a row" as definitive proof of a broken
	// generator and bail immediately at Error (faster signal, no further
	// log spam), and let the final fall-through error carry the post-loop
	// fail signal for the rare case where 10 distinct IDs all collided
	// against existing jobs.
	prevID := j.ID
	for i := 0; i < 10; i++ {
		if _, exists := s.jobs[j.ID]; !exists {
			break
		}
		if i == 0 {
			slog.Warn("cron: job ID collision, retrying", "attempt", i+1, "job_id", j.ID)
		}
		retryID, retryErr := generateID()
		if retryErr != nil {
			// 同上：rand 中途失效，提早返回比继续循环更诚实。
			return nil, addJobStubFields{}, 0, fmt.Errorf("cron: regenerate job id (retry %d): %w", i+1, retryErr)
		}
		if retryID == prevID {
			// Deterministic generator: same ID twice in a row is conclusive
			// evidence the source is not random. No point exhausting the
			// remaining retries; the final error would be identical.
			slog.Error("cron: deterministic ID generator detected; bailing early",
				"attempt", i+1, "id", retryID)
			return nil, addJobStubFields{}, 0, fmt.Errorf("cron: deterministic ID generator (id %q repeated)", retryID)
		}
		prevID = retryID
		j.ID = retryID
	}
	if _, exists := s.jobs[j.ID]; exists {
		return nil, addJobStubFields{}, 0, fmt.Errorf("cron: failed to generate unique job ID after 10 attempts")
	}
	j.CreatedAt = time.Now()

	if !j.Paused {
		if err := s.registerJob(j); err != nil {
			return nil, addJobStubFields{}, 0, err
		}
	}
	s.jobs[j.ID] = j
	// R237-PERF-5 / R242-GO-9 (#661 / #558): increment the per-chat counter
	// and append to the per-chat index synchronously with s.jobs so the next
	// addJobAcquiringLock observes the up-to-date count without re-scanning and
	// findByPrefixLocked iterates only this chat's jobs. deleteJobLocked is the
	// paired inverse; the rollback path (s.deleteJobLocked below on persist
	// failure) unwinds both correctly.
	s.addToChatIndexLocked(j)
	save, perr := s.persistJobsLocked()
	if perr != nil {
		// R236-GO-10: persist failed *after* registerJob + map insertion.
		// Without rollback, the in-memory state holds an orphan: cron
		// scheduler has the entry, s.jobs has the *Job, but disk has
		// nothing — every tick logs "job not found" then never cleans
		// up because the cron entry stays registered (the dispatcher's
		// debug log path doesn't call s.cron.Remove). Rolling back
		// via deleteJobLocked unwinds the cron entry and map entry
		// under the still-held s.mu, so the persistence gap surfaces
		// as a clean failure to the caller and a fresh AddJob on the
		// same ID is safe. Earlier review note worried about another
		// goroutine observing the entry between registerJob and
		// persist; that window is enclosed by s.mu (the cron
		// dispatcher's tick fans out via runningJobs CAS without
		// re-entering s.mu for lookup, but execute()'s s.jobs[j.ID]
		// read does take s.mu — see executeJob). So the rollback is
		// observationally consistent.
		//
		// R240-GO-1: deleteJobLocked no longer touches the router
		// stub; in this rollback path the stub was never registered
		// (registerStubFromJob runs in AddJob *after* this helper
		// returns and after a successful save), so no router-side
		// cleanup is needed. resetRouterStub on a never-registered
		// key would be a no-op anyway.
		//
		// R20260605B-CORR-6 (#1810): deleteJobLocked now snapshots+zeros the
		// cron entryID and returns it instead of calling s.cron.Remove under
		// s.mu. Surface it as rollbackEntryID so AddJob removes the orphaned
		// cron entry AFTER releasing s.mu — keeping the unbuffered c.remove
		// channel round-trip off the write-lock hold.
		rollbackEntryID = s.deleteJobLocked(j)
		return nil, addJobStubFields{}, rollbackEntryID, perr
	}
	// R250-GO-5 (#1068): snapshot the fields registerStubByValue reads under
	// s.mu so AddJob can call it without re-reading *j after lock release.
	// Mirrors UpdateJob (scheduler_jobs.go ~L955) and SetJobPrompt's value-
	// pass pattern; closes the documented "passing *Job after lock release"
	// drift hazard from R232-CR-12 (scheduler.go:1163-1165).
	stub = addJobStubFields{
		id:            j.ID,
		workDir:       j.WorkDir,
		prompt:        j.Prompt,
		lastSessionID: j.LastSessionID,
	}
	return save, stub, 0, nil
}

// PerChatJobCount returns the number of jobs registered against the
// (Platform, ChatID) chat. Backed by s.chatJobCount (R237-PERF-5 / #661):
// O(1) read, lock-free outside the RLock window, vs the historical
// O(N) scan that addJobAcquiringLock used to enforce maxJobsPerChat.
//
// Intended use: dashboard / metrics surfaces that want to render
// "you have N/M cron jobs in this chat" without paying the cost of a
// full ListJobs walk. Returns 0 for a chat with no registered jobs.
//
// Safe on a nil *Scheduler (returns 0) so dashboard renders during
// the bootstrap window before the scheduler is wired do not NPE.
func (s *Scheduler) PerChatJobCount(plat, chatID string) int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.chatJobCount[chatKeyFor(plat, chatID)]
}

// ListJobs returns jobs for a specific chat.
//
// R20260527-PERF-1: walks the jobsByChat[(plat, chatID)] index instead of
// scanning the entire s.jobs map — O(jobs-in-chat) vs the historical O(N).
// Matters once a deployment accumulates many cron jobs across many chats:
// dashboard list polls hit ListJobs at 1 Hz per active chat.
func (s *Scheduler) ListJobs(plat, chatID string) []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	bucket := s.jobsByChat[chatKeyFor(plat, chatID)]
	// R247-GO-3: pre-allocate so an empty result marshals as `[]` instead of
	// `null` — keeps the JSON wire-format consistent with ListAllJobsWithNextRun
	// and frontend `.length` defenders unaffected. [BREAKING-LOCAL]
	result := make([]Job, 0, len(bucket))
	for _, j := range bucket {
		result = append(result, *j)
	}
	return result
}

// JobWithNextRun pairs a Job snapshot with its next scheduled run time so
// callers rendering lists (dashboard) don't need a second round-trip per job.
type JobWithNextRun struct {
	Job     Job
	NextRun time.Time
}

// ListJobsWithNextRun returns the jobs for a specific chat plus each job's
// next scheduled run — the chat-narrowed twin of ListAllJobsWithNextRun.
//
// R249-CR-12 (#956): ListJobs returns chat-scoped []Job WITHOUT NextRun, while
// ListAllJobsWithNextRun returns NextRun but for EVERY job. A caller wanting
// "this chat's jobs + their NextRun" previously had to call the all-jobs
// variant and filter — paying an O(allJobs) walk to render an O(jobs-in-chat)
// list. This helper closes that asymmetry: it walks the jobsByChat bucket
// (O(jobs-in-chat)) and patches NextRun in.
//
// Lock strategy mirrors ListAllJobsWithNextRun: snapshot (Job copy, entryID)
// under s.mu.RLock, release s.mu, then read s.cron.Entries() lock-free to
// avoid inverting the cron dispatcher's lock order (cron-internal → execute →
// s.mu.Lock). The result slice is always non-nil (empty bucket → `[]`) for
// wire-format symmetry with ListJobs / ListAllJobsWithNextRun.
func (s *Scheduler) ListJobsWithNextRun(plat, chatID string) []JobWithNextRun {
	// R20260602-CR-3: reuse listEntryIDsPool for the per-chat entryID
	// scratch slice — the same pool ListAllJobsWithNextRun uses — so 1Hz
	// dashboard polls pay zero allocs for the transient ids buffer.
	idsPtr := listEntryIDsPool.Get().(*[]cronEntryID)
	ids := (*idsPtr)[:0]
	defer func() {
		*idsPtr = ids[:0]
		listEntryIDsPool.Put(idsPtr)
	}()

	s.mu.RLock()
	bucket := s.jobsByChat[chatKeyFor(plat, chatID)]
	result := make([]JobWithNextRun, 0, len(bucket))
	if cap(ids) < len(bucket) {
		ids = make([]cronEntryID, 0, len(bucket))
	}
	for _, j := range bucket {
		result = append(result, JobWithNextRun{Job: *j})
		ids = append(ids, j.entryID)
	}
	s.mu.RUnlock()

	if len(result) == 0 {
		return result
	}

	// Single Entries() snapshot read outside s.mu (lock-order safe). entryID 0
	// means the job is not registered with cron (paused) and keeps the zero
	// NextRun.
	//
	// R20260602141221-PERF-2 (#1583): the lookup is O(jobs-in-chat × |entries|)
	// — a linear scan of the full Entries() snapshot per job. For the common
	// per-chat bucket (1-5 jobs) the scan is cheaper than building a map: a
	// handful of scans over |entries| beats paying the map's hashing + (even
	// pooled) bookkeeping, and avoids touching the shared listNextByIDPool. But
	// when a chat accumulates many jobs the product blows up (jobs × |entries|,
	// |entries|≤500) and the dashboard's 1Hz poll amplifies it. Above the
	// threshold we switch to the same pooled entryID→Next map ListAllJobsWithNextRun
	// uses — O(|entries|) build + O(1) per job — so both the small-bucket and
	// large-bucket cases stay cheap. The threshold is deliberately small: by a
	// handful of jobs the map's single Entries() walk already wins on the
	// comparison count and the constant-factor crossover is well behind us.
	entries := s.cron.Entries()
	if len(result) <= listNextRunMapThreshold {
		for i, id := range ids {
			if id == 0 {
				continue
			}
			for _, e := range entries {
				if e.ID == id {
					result[i].NextRun = e.Next
					break
				}
			}
		}
		return result
	}

	nextByIDPtr := listNextByIDPool.Get().(*map[cronEntryID]time.Time)
	nextByID := *nextByIDPtr
	clear(nextByID)
	defer func() {
		clear(nextByID)
		listNextByIDPool.Put(nextByIDPtr)
	}()
	for _, e := range entries {
		nextByID[e.ID] = e.Next
	}
	for i, id := range ids {
		if id != 0 {
			result[i].NextRun = nextByID[id]
		}
	}
	return result
}

// listNextRunMapThreshold is the per-chat job count at or below which
// ListJobsWithNextRun uses a direct linear Entries() scan rather than building
// the pooled entryID→Next map. Below it the scan's small constant factor wins;
// above it the O(jobs × |entries|) product makes the O(|entries|) map build
// cheaper. R20260602141221-PERF-2 (#1583). 8 is comfortably past the common
// 1-5 jobs/chat bucket so the typical dashboard poll keeps the allocation-free
// linear path, while a chat that hoards jobs no longer scales quadratically.
const listNextRunMapThreshold = 8

// ListAllJobsWithNextRun returns every job plus its next scheduled run.
// Lock strategy: snapshot (*Job, entryID) under s.mu.RLock, release s.mu, then
// call s.cron.Entries() without holding s.mu. Calling cron.Entries inside
// s.mu would invert the lock order taken by the cron dispatcher path
// (cron-internal → execute → recordResultP0WithSanitised → s.mu.Lock),
// risking a deadlock.
//
// R236-PERF-11: this used to call s.cron.Entry(id) per job, but
// robfig/cron v3's Entry is implemented as `for _, e := range Entries()
// { if e.ID == id }` and Entries() takes runningMu — so N jobs cost
// N×N entry walks plus N mutex acquisitions on the dispatcher's mutex.
// Building one entryID→Next map up front collapses the cost to O(N) and
// a single mutex acquisition, which matters when the dashboard list API
// polls at 1 Hz with 50 jobs registered.
func (s *Scheduler) ListAllJobsWithNextRun() []JobWithNextRun {
	// R247-PERF-4 (#530) / R250-PERF-15 (#1118): the dashboard list endpoint
	// polls this at 1Hz across N open tabs, so we pool the two transient
	// containers (entryID scratch + EntryID-keyed nextRun map) to keep
	// per-poll alloc count flat as job count grows. The result slice is owned
	// by the caller and stays heap-resident, so it is NOT pooled — and each
	// Job is copied straight into it under RLock so there is no second copy.
	idsPtr := listEntryIDsPool.Get().(*[]cronEntryID)
	ids := (*idsPtr)[:0]
	defer func() {
		// Reset length but keep capacity so the next call skips the make.
		*idsPtr = ids[:0]
		listEntryIDsPool.Put(idsPtr)
	}()

	var result []JobWithNextRun
	s.mu.RLock()
	if cap(ids) < len(s.jobs) {
		ids = make([]cronEntryID, 0, len(s.jobs))
	}
	result = make([]JobWithNextRun, 0, len(s.jobs))
	for _, j := range s.jobs {
		// Single Job copy: directly into the caller-owned result. NextRun is
		// patched in by index below once Entries() has been read lock-free.
		result = append(result, JobWithNextRun{Job: *j})
		ids = append(ids, j.entryID)
	}
	s.mu.RUnlock()

	// Single Entries() snapshot → entryID-keyed map. The map is pooled and
	// `clear()`-ed before re-Put so stale keys from a previous larger
	// snapshot do not leak into a smaller current one. The alternative —
	// re-walking Entries per job — is O(N²) and re-acquires runningMu per
	// Entry() call. Called outside s.mu to avoid inverting the lock order
	// the cron dispatcher takes (cron-internal → execute → s.mu.Lock).
	entries := s.cron.Entries()
	nextByIDPtr := listNextByIDPool.Get().(*map[cronEntryID]time.Time)
	nextByID := *nextByIDPtr
	clear(nextByID)
	defer func() {
		clear(nextByID)
		listNextByIDPool.Put(nextByIDPtr)
	}()
	for _, e := range entries {
		nextByID[e.ID] = e.Next
	}

	for i, id := range ids {
		if id != 0 {
			result[i].NextRun = nextByID[id]
		}
	}
	return result
}

// addToChatIndexLocked records a job into the two per-chat side indexes that
// must move in lockstep with s.jobs: the chatJobCount cap counter and the
// jobsByChat prefix-lookup slice. Caller MUST hold s.mu.Lock() and have
// already inserted j into s.jobs.
//
// R237-PERF-5 / R242-GO-9 (#661 / #558): both indexes are the paired inverse
// of deleteJobLocked's decrement+swap-shrink. R249-CR-4 / R260528-ARCH-7
// (#948 / #1368): the identical two-line increment+append was open-coded at
// the AddJob path and twice in Start's load loop, so the "mutated together so
// they never drift" invariant the Scheduler godoc promises lived only as a
// comment. Folding it into one helper makes the invariant structural — a
// future third index lands here once instead of drifting across the three
// insertion sites.
func (s *Scheduler) addToChatIndexLocked(j *Job) {
	key := chatKeyFor(j.Platform, j.ChatID)
	s.chatJobCount[key]++
	s.jobsByChat[key] = append(s.jobsByChat[key], j)
	s.insertSortedJobID(j.ID)
}

// insertSortedJobID keeps s.sortedJobIDs in ascending order via a binary-
// search insertion so marshalJobsLocked can iterate it without re-sorting on
// every mutation. O(N) memmove for the shift dominates over the O(log N)
// search; at maxJobsHardCap=500 this is far cheaper than the prior
// O(N log N) slices.SortFunc on every persist. Idempotent on a duplicate ID
// (AddJob rejects collisions before insertion, but the disk-load path could
// in principle replay a malformed file with dup IDs — a no-op keeps the slice
// 1:1 with the map). Caller must hold s.mu.Lock(). R164029-PERF-9 (#1598).
func (s *Scheduler) insertSortedJobID(id string) {
	i, found := slices.BinarySearch(s.sortedJobIDs, id)
	if found {
		return
	}
	s.sortedJobIDs = slices.Insert(s.sortedJobIDs, i, id)
}

// removeSortedJobID drops id from s.sortedJobIDs via binary search, preserving
// sort order. No-op if absent so a double-delete (rollback path) cannot panic.
// Caller must hold s.mu.Lock(). R164029-PERF-9 (#1598).
func (s *Scheduler) removeSortedJobID(id string) {
	if i, found := slices.BinarySearch(s.sortedJobIDs, id); found {
		s.sortedJobIDs = slices.Delete(s.sortedJobIDs, i, i+1)
	}
}

// deleteJobLocked performs the in-memory side effects of removing a job:
// snapshot+zero the cron entry and drop the map entry. It returns the
// captured cron entryID so the caller can run s.cron.Remove AFTER releasing
// s.mu — it intentionally does NOT call s.cron.Remove itself.
//
// Caller must hold s.mu.Lock() and pass a non-nil job that exists in
// s.jobs. Intentionally does NOT delete from s.runningJobs: a concurrent
// execute() for this job may still hold the atomic.Bool and be about to
// CAS it back to false; if a fresh AddJob somehow reused the same ID
// (low but non-zero given the hex8 generator), creating a new guard entry
// here could split the CAS gate between two goroutines and permit double
// execution. Retaining the entry is bounded by maxJobsHardCap (one
// *atomic.Bool per historical job) — cheap vs a correctness gap. R219-CR-4.
//
// R240-GO-1: router.Reset MUST NOT be called from inside this function
// because router.Reset → notifyChange callbacks may attempt to acquire
// s.mu, leading to lock-order inversion / recursive write-lock deadlock.
// Callers are responsible for calling resetRouterStub(j.ID) AFTER they
// release s.mu. EnsureStub's godoc already documents the same
// "must-not-hold-s.mu" contract; this function now respects it.
//
// R20260605B-CORR-6 (#1810): s.cron.Remove is likewise hoisted out of the
// s.mu hold. When the cron is running, Remove sends on the unbuffered
// c.remove channel that only run() drains, so calling it under s.mu held the
// write lock across a cron-select round-trip — the same hazard pauseJobLocked
// / resumeJobLocked / UpdateJob already hoist their Remove for. We snapshot
// and zero j.entryID under lock (so a concurrent ListAllJobsWithNextRun /
// NextRun snapshot sees the entry-removed state immediately) and return it;
// the caller removes it from cron after Unlock. Returns 0 when there was no
// cron entry, so callers can call s.cron.Remove unconditionally (Remove of 0
// is a no-op).
func (s *Scheduler) deleteJobLocked(j *Job) (removeEntryID cronEntryID) {
	removeEntryID = j.entryID
	j.entryID = 0
	if _, present := s.jobs[j.ID]; present {
		delete(s.jobs, j.ID)
		// R164029-PERF-9 (#1598): paired removal from the sorted-ID slice,
		// mirroring addToChatIndexLocked's insert. Guarded by the same
		// membership check so a double-delete cannot disturb the slice.
		s.removeSortedJobID(j.ID)
		// R237-PERF-5 (#661): paired decrement for the addJobAcquiringLock
		// increment. Guarded by the s.jobs membership check above so a
		// double-delete (rollback path calling this on a never-inserted
		// job, or a future caller hitting this twice) cannot drive the
		// counter negative — divergence from s.jobs would silently disable
		// the per-chat cap. Drop the entry when count hits zero so the
		// map's working set tracks the live chat set rather than every
		// chat that has ever owned a job.
		key := chatKeyFor(j.Platform, j.ChatID)
		if n := s.chatJobCount[key]; n > 1 {
			s.chatJobCount[key] = n - 1
		} else {
			delete(s.chatJobCount, key)
		}
		// R242-GO-9 (#558): paired remove from per-chat job index. Swap-
		// and-shrink to keep amortised O(1) (insertion-order is not
		// preserved, which is fine because findByPrefixLocked already
		// reports an ambiguous-prefix error rather than picking a winner
		// when two jobs share the prefix). Drop the entry when the slice
		// empties so the map's working set tracks the live chat set,
		// mirroring the chatJobCount cleanup above.
		if list := s.jobsByChat[key]; len(list) > 0 {
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
				delete(s.jobsByChat, key)
			} else {
				s.jobsByChat[key] = list
			}
		}
	}
	return removeEntryID
}

// deleteJobPostCleanup runs the lock-free side effects that must follow a
// deleteJobLocked, in the exact order both delete entry points
// (DeleteJobByID and DeleteJob) require. Caller MUST NOT hold s.mu — every
// step here is documented as a "must-not-hold-s.mu" operation:
//
//   - resetRouterStub: router.Reset → notifyChange callbacks may re-enter
//     s.mu (R240-GO-1 lock-order inversion guard).
//   - runStore.DeleteJob: fires even when the earlier persistJobsLocked
//     failed so runs/<jobID>/ does not leak on disk while the in-memory
//     record is already gone (R238-GO-3). Gated on enabled() so a disabled
//     store is a no-op.
//   - cleanupRunningJobIfIdle: reclaims the s.runningJobs guard when the CAS
//     gate is idle, bounding what would otherwise be an unbounded *runInflight
//     leak per historical jobID (R242-ARCH-15 / #758).
//
// R244-ARCH-13 / R244-ARCH-19 (#1053 / #1056): the two delete entry points
// previously open-coded this identical three-step closure. Folding it into
// one helper means a future change to the delete side-effect order (or a new
// step) lands once instead of drifting between the ID-based and
// plat+chat-based mutator pipelines.
//
// R20260605B-CORR-6 (#1810): removeEntryID is the cron entryID snapshotted by
// deleteJobLocked under s.mu. s.cron.Remove runs here (post-unlock) so the
// unbuffered c.remove channel send no longer blocks the s.mu write hold;
// Remove(0) is a no-op so callers pass 0 when the job had no cron entry.
func (s *Scheduler) deleteJobPostCleanup(jobID string, removeEntryID cronEntryID) {
	if removeEntryID != 0 {
		s.cron.Remove(removeEntryID)
	}
	s.resetRouterStub(jobID)
	s.deleteJobRuns(jobID)
	s.cleanupRunningJobIfIdle(jobID)
}

// pauseJobLocked transitions a job to Paused state under s.mu. Returns
// ErrJobAlreadyPaused without mutation if the job is already paused so
// the caller can map it to 409 Conflict. R219-CR-4.
//
// R236-QA-03 (#537): the historical implementation called s.cron.Remove
// while holding s.mu, mirroring the lock-order risk that
// ListAllJobsWithNextRun's godoc explicitly warns against — robfig/cron's
// Remove takes c.runningMu and synchronously sends on the unbuffered
// c.remove channel, so the caller's s.mu hold time bounded by however
// long the cron run goroutine takes to come back to its select. The
// in-memory mutation is now done under lock; the cron.Remove is hoisted
// to a post-unlock callback callers must invoke (mirrors the
// router.Reset move-out-of-deleteJobLocked pattern from R240-GO-1).
//
// All callers are now responsible for running the returned `cronCleanup`
// closure AFTER releasing s.mu. cronCleanup is non-nil even on the no-op
// path (j.entryID was zero), so callers can defer it unconditionally
// without nil-checking. cronCleanup is idempotent — re-running it after
// a successful first call is a no-op (the captured entryID is consumed
// at the first call, so subsequent calls hit the entryID==0 fast path).
func (s *Scheduler) pauseJobLocked(j *Job) (cronCleanup func(), err error) {
	if j.Paused {
		return func() {}, fmt.Errorf("%w: id %q", ErrJobAlreadyPaused, j.ID)
	}
	// Snapshot the entryID we'll remove from cron AFTER s.mu is released.
	// Set j.entryID = 0 under lock so any concurrent ListAllJobsWithNextRun
	// / NextRun / TriggerNow snapshotting the job sees the entry-removed
	// state immediately, even before cron's internal table catches up.
	captured := j.entryID
	j.entryID = 0
	j.Paused = true
	if captured == 0 {
		return func() {}, nil
	}
	return func() { s.cron.Remove(captured) }, nil
}

// resumeJobLocked transitions a paused job back to active under s.mu by
// re-registering the cron entry. Returns ErrJobNotPaused without mutation
// if the job is not paused, or registerJob's error if re-registration
// fails (e.g. schedule no longer parses) — leaving Paused=true so the
// caller can retry. R219-CR-4.
func (s *Scheduler) resumeJobLocked(j *Job) error {
	if !j.Paused {
		return fmt.Errorf("%w: id %q", ErrJobNotPaused, j.ID)
	}
	if err := s.registerJob(j); err != nil {
		return err
	}
	j.Paused = false
	return nil
}

// withJobByID 是 DeleteJobByID / PauseJobByID / ResumeJobByID 三 dashboard
// 入口的共用执行框架。R247-CR-1：原本三函数 ~120 行重复 closure + 持锁 +
// persist + unlock-then-save 逻辑，本 helper 收口为 3 阶段：
//
//  1. 持 s.mu.Lock 查 id；缺失即返回 ErrJobNotFound 包装错误；
//  2. 调 op(j) 执行业务变更（可返回 op-specific 错误而无 mutation）；
//     op 成功后 persistJobsLocked 拿 save 闭包；
//  3. 释放 s.mu，调 postCleanup(j)（router.Reset / runStore.DeleteJob
//     之类需在锁外的副作用），然后 save() 落盘。
//
// op 在 s.mu.Lock 下执行；postCleanup 在 s.mu 释放后执行。op 返回
// 非 nil 错误时 perr 透传给上层，且 postCleanup 不会被调用。op == nil
// 表示纯删除/查询无业务校验（DeleteJobByID 用此）。postCleanup == nil
// 表示无锁外副作用（Pause/Resume 用此）。
//
// 返回三元组 (*Job, error)：
//   - 找不到：(nil, ErrJobNotFound 包装)；
//   - op 失败：(nil, op 返回的 err)；
//   - persist 失败：(nil, ErrPersistFailed 包装)；postCleanup 已执行。
//   - 成功：(*Job, nil)。
//
// R241-GO-2/3 的"explicit found/ok"语义在此聚合：内部用 found 区分
// 找不到 vs op 失败，调用方不再重复 if j == nil 的歧义判断。
//
// R242-GO-3 (#548)：返回的 *Job 是 in-lock 时刻 *j 的 value-copy 的地址，
// 不再是 s.jobs[id] 的活指针。原本 j 被赋为 s.jobs[id] 后随 s.mu.Unlock
// 一起返回给调用方，调用方在锁外读取的 j.Field 可能与另一个 goroutine 的
// UpdateJob/SetJobPrompt 并发，触发 string header tear / data race。
// UpdateJob (line 655) 的 critical section 已经在锁内做 *j 复制，本 helper
// 把同样语义铺到 Delete/Pause/Resume 三入口；postCleanup 仍然收到锁外
// 拿到的 *jobSnapshot，副作用（router.Reset / runStore.DeleteJob）只读
// snapshot 的不可变字段（ID/Platform/ChatID）所以语义不变。
// withJobByIDOpts bundles the optional knobs withJobByID accepts so the
// signature stays a single function while individual callers (Pause /
// Resume / Delete / future ops) opt in to rollback semantics without
// touching unrelated paths. R20260527-COR-1 (#1272): historically op-
// success + persist-failure left in-memory state mutated and on-disk
// state stale, so a restart replayed the pre-op snapshot — divergence
// most visible on PauseJobByID (cron entry gone, j.Paused=true in
// memory, but disk shows Paused=false). When rollbackOnPersistErr is
// non-nil and persistJobsLocked returns an error, the helper invokes
// it under s.mu BEFORE releasing — restoring the in-memory mutation
// to match the un-persisted disk state — and skips postCleanup so the
// mutation's lock-released side effects (cron.Remove / router.Reset)
// don't fire on a rolled-back op.
//
// R20260527-GO-8 (#1300) op contract：op MUST be one of：
//
//  1. 全 mutate 成功 → return nil；in-memory 状态一致，persistJobsLocked
//     紧接其后落盘（marshal 失败时见 R20260527-COR-1 / #1272 的退路语义）。
//  2. 全无 mutate 失败 → return non-nil error；contract 是 op MUST NOT
//     leave any partial mutation on *j when returning error。否则 perr
//     透传给调用方但 in-memory 已脏 + persist 未触发，重启后状态发散。
//
// 现有 op 实现（pauseJobLocked / resumeJobLocked / deleteJobLocked-wrap）
// 均满足该不变量：失败检查放在所有写入之前。新增 op 时 reviewer 必须验
// 证：op 函数体内任意 return non-nil 路径之前没有 j.X = ... 写入；如果
// op 需要先尝试再回滚，应在 op 内部完成回滚后再 return。
// withJobByIDOpts knobs:
//
//   - op: in-lock mutation (must satisfy "all-or-nothing" — see contract
//     above). nil for pure-lookup callers.
//   - postCleanup: out-of-lock side effect (router.Reset, runStore.DeleteJob)
//     that runs UNCONDITIONALLY whenever op succeeded — even when
//     persistJobsLocked returned an error and rollbackOnPersistErr is nil.
//     Use this shape ONLY when the in-lock mutation is already past the
//     point of no return (DeleteJobByID's deleteJobLocked drops the *Job
//     from s.jobs irreversibly). For mutations where on-disk state is the
//     authoritative outcome (Pause/Resume), pair op with rollbackOnPersistErr
//     and leave postCleanup nil. R250-CR-16 (#1149).
//   - rollbackOnPersistErr: in-lock undo of the op's mutation when
//     persistJobsLocked fails. When non-nil and persist fails, this
//     restores *j BEFORE the snapshot copy and skips postCleanup so the
//     caller observes "no change applied".
//
// R249-ARCH-20 (#985): op and the two side-effect hooks share the bare
// `func(*Job)` / `func(*Job) error` shape, so a reviewer eyeballing a call
// site had nothing but argument position to tell an in-lock mutation apart
// from an out-of-lock cleanup. Naming them (lockedJobOp returns an error and
// runs UNDER s.mu; jobSideEffect returns nothing and runs lock-free) makes the
// two roles self-documenting and lets the compiler flag a swapped op-vs-cleanup
// argument (an error-returning closure can no longer be passed where a
// jobSideEffect is expected, and vice versa). Behaviour is unchanged — Go
// closures still satisfy these named types by assignability at every call
// site, so no call-site edits were needed.
type (
	// lockedJobOp is the in-lock mutation withJobByID(Opt) / withJobByPrefix
	// run while holding s.mu. It MUST be all-or-nothing: on a non-nil error
	// return it must leave *j unmutated (see the op contract godoc above).
	lockedJobOp func(j *Job) error
	// jobSideEffect is an out-of-lock hook (postCleanup / rollbackOnPersistErr).
	// It runs after s.mu is released (postCleanup) or as the in-lock undo of a
	// failed persist (rollbackOnPersistErr), and returns nothing.
	jobSideEffect func(j *Job)
)

type withJobByIDOpts struct {
	op                   lockedJobOp
	postCleanup          jobSideEffect
	rollbackOnPersistErr jobSideEffect
}

func (s *Scheduler) withJobByID(
	id string,
	op lockedJobOp,
	postCleanup jobSideEffect,
) (*Job, error) {
	return s.withJobByIDOpt(id, withJobByIDOpts{op: op, postCleanup: postCleanup})
}

// withJobByIDResult bundles the values withJobByIDOpt's locked critical
// section produces so the post-unlock control flow reads as named-field
// branches rather than five sibling `var` declarations mutated inside an
// IIFE. R249-CR-7 (#951): the prior shape declared save/snapshot/found/
// opErr/perr/rolledBack up front and assigned them from a closure, forcing
// the reader to scan both the IIFE body and the trailing branch ladder to
// reconstruct the state machine. Folding the locked work into
// lockedJobOp keeps the s.mu critical section in one named method and lets
// the caller branch on the returned struct.
type withJobByIDResult struct {
	save       func()
	snapshot   Job
	found      bool
	opErr      error
	perr       error
	rolledBack bool
}

// lockedJobOp runs the lookup + op + persist + (optional) rollback steps for
// withJobByIDOpt entirely under s.mu and returns the outcome. Splitting this
// out of the IIFE keeps every s.mu-guarded mutation in one named scope; the
// caller (withJobByIDOpt) is then pure post-unlock control flow.
func (s *Scheduler) lockedJobOp(id string, opts withJobByIDOpts) withJobByIDResult {
	var r withJobByIDResult
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		r.perr = fmt.Errorf("%w: id %q", ErrJobNotFound, id)
		return r
	}
	if opts.op != nil {
		if err := opts.op(j); err != nil {
			r.opErr = err
			return r
		}
	}
	r.found = true
	r.save, r.perr = s.persistJobsLocked()
	// R20260527-COR-1 (#1272): if the marshal step failed AFTER op
	// mutated *j, restore the in-memory mutation under s.mu so on-
	// disk state and in-memory state stay aligned. Run the rollback
	// before snapshotting so the returned snapshot reflects the
	// pre-op state — caller observes "no change applied" rather than
	// the half-applied mutation that motivated the divergence bug.
	if r.perr != nil && opts.rollbackOnPersistErr != nil {
		opts.rollbackOnPersistErr(j)
		r.rolledBack = true
	}
	// R242-GO-3 (#548): value-copy under s.mu so the caller (and
	// postCleanup) read a stable Job even if a concurrent
	// UpdateJob / SetJobPrompt mutates the live *j right after we
	// unlock. Mirrors UpdateJob's `return *j, save, perr` pattern.
	r.snapshot = *j
	return r
}

func (s *Scheduler) withJobByIDOpt(id string, opts withJobByIDOpts) (*Job, error) {
	r := s.lockedJobOp(id, opts)
	save, snapshot, found, perr, rolledBack := r.save, r.snapshot, r.found, r.perr, r.rolledBack

	if r.opErr != nil {
		return nil, r.opErr
	}
	if !found {
		return nil, perr
	}
	// R20260527-COR-1 (#1272): on rollback, skip postCleanup — its side
	// effects (cron.Remove for Pause, router.Reset for Delete) reflect a
	// mutation that is no longer in effect. Returning perr lets the caller
	// surface the persist failure as 5xx so the operator can retry.
	if rolledBack {
		return nil, perr
	}
	// R250-CR-16 (#1149): postCleanup runs UNCONDITIONALLY here — i.e.
	// even when persistJobsLocked returned perr != nil (without a paired
	// rollbackOnPersistErr hook). This is INTENTIONAL for DeleteJobByID:
	// deleteJobLocked already ran inside the locked section and dropped
	// the *Job from s.jobs, so the in-memory state is already past the
	// point of no return. The runStore.DeleteJob cleanup MUST run even
	// when the cron_jobs.json marshal failed; otherwise runs/<jobID>/
	// would leak entries for a job nobody can address again from the
	// dashboard (R238-GO-3). PauseJobByID / ResumeJobByID pass nil
	// postCleanup so this branch is a no-op for them — the asymmetry
	// is by design, not by accident. Future maintainers adding a new
	// withJobByID-shaped op MUST decide explicitly: cleanup after
	// success-only (use rollbackOnPersistErr to undo on failure) vs
	// cleanup-regardless (the DeleteJob shape, where the in-memory
	// mutation is already irreversibly applied).
	if opts.postCleanup != nil {
		opts.postCleanup(&snapshot)
	}
	if perr != nil {
		return nil, perr
	}
	save()
	return &snapshot, nil
}

// DeleteJobByID removes a job by exact ID (unscoped, for dashboard use).
func (s *Scheduler) DeleteJobByID(id string) (*Job, error) {
	// R20260605B-CORR-6 (#1810): capture the cron entryID deleteJobLocked
	// snapshots under s.mu and remove it from cron in postCleanup (lock
	// released) so the unbuffered c.remove channel round-trip no longer
	// happens under the s.mu write hold — matching the pause/resume hoist.
	var removeEntryID cronEntryID
	return s.withJobByID(
		id,
		// op：调 deleteJobLocked 移除 in-memory 记录；不返回错误（删除路径无校验）。
		func(j *Job) error {
			removeEntryID = s.deleteJobLocked(j)
			return nil
		},
		// postCleanup：锁外做 cron.Remove + router.Reset + runStore.DeleteJob +
		// runningJobs reclaim。R244-ARCH-13 (#1053): 共享 helper，与
		// plat+chat-based DeleteJob 走同一条 side-effect 顺序，详见
		// deleteJobPostCleanup godoc（R240-GO-1 / R238-GO-3 / R242-ARCH-15）。
		func(j *Job) { s.deleteJobPostCleanup(j.ID, removeEntryID) },
	)
}

// PauseJobByID pauses a job by exact ID (unscoped, for dashboard use).
//
// R236-QA-03 (#537): cron.Remove is hoisted to postCleanup so s.mu is
// released before the unbuffered c.remove channel send completes —
// matches the lock-order discipline ListAllJobsWithNextRun's godoc
// pins. The closure captures the cleanup func returned by
// pauseJobLocked under s.mu so the entryID we're removing is the
// exact one snapshotted at the in-memory mutation point (no
// re-read race after Unlock).
//
// R20260527-COR-1 (#1272): if persistJobsLocked fails AFTER pauseJobLocked
// flipped j.Paused / cleared j.entryID, the disk-vs-memory divergence
// surfaces on restart as the unpaused job replaying from disk. Capture
// the pre-op (entryID, Paused) tuple so a rollback callback can restore
// in-memory state to match the un-persisted disk view; the helper also
// skips postCleanup (the cron.Remove hoist) on rollback so the cron entry
// stays alive and the next tick still fires the now-active job.
func (s *Scheduler) PauseJobByID(id string) (*Job, error) {
	var pauseCleanup func()
	var prevEntryID cronEntryID
	var prevPaused bool
	var captured bool
	op := func(j *Job) error {
		// Snapshot under s.mu so the rollback restores the exact
		// pre-op view; pauseLocked mutates j.entryID + j.Paused only
		// after this read so a concurrent reader can never observe a
		// torn write.
		prevEntryID = j.entryID
		prevPaused = j.Paused
		captured = true
		c, err := s.pauseJobLocked(j)
		pauseCleanup = c
		return err
	}
	postCleanup := func(_ *Job) {
		if pauseCleanup != nil {
			pauseCleanup()
		}
	}
	rollback := func(j *Job) {
		// Only restore if op actually ran and captured the pre-op view —
		// guards against a future refactor that might invoke rollback on
		// the "op never ran" path.
		if !captured {
			return
		}
		j.entryID = prevEntryID
		j.Paused = prevPaused
		// pauseCleanup is the cron.Remove hoist returned by pauseJobLocked.
		// Drop it so postCleanup's safety net (which is now skipped on
		// rollback in withJobByIDOpt) cannot accidentally fire even if a
		// future refactor reorders the flow.
		pauseCleanup = nil
	}
	return s.withJobByIDOpt(id, withJobByIDOpts{
		op:                   op,
		postCleanup:          postCleanup,
		rollbackOnPersistErr: rollback,
	})
}

// ResumeJobByID resumes a paused job by exact ID (unscoped, for dashboard use).
//
// R20260526-GO-001 (#1226): registerJob mutates j.entryID + j.cachedPeriod
// before resumeJobLocked flips j.Paused, so a persistJobsLocked failure
// after op-success would leave in-memory state with a live cron entry +
// Paused=false while disk still shows Paused=true — restart would then
// re-register the schedule on top of the surviving runtime entry,
// producing a double-fire. Capture the pre-op state under s.mu and
// install a rollback that removes the cron entry and restores
// (entryID, cachedPeriod, Paused) so the in-memory view matches the
// un-persisted disk view. Mirrors PauseJobByID's rollback contract.
func (s *Scheduler) ResumeJobByID(id string) (*Job, error) {
	var prevEntryID cronEntryID
	var prevCachedPeriod time.Duration
	var prevCachedSched robfigcron.Schedule
	var prevPaused bool
	var captured bool
	// CR-1 (R250531-CR-1): entryID to remove AFTER withJobByIDOpt returns
	// (i.e. after s.mu is released). rollback runs under s.mu; calling
	// s.cron.Remove there causes a lock-order inversion — robfig/cron.Remove
	// sends on the unbuffered c.remove channel which can only be drained by
	// the cron-tick goroutine, and that goroutine calls executeJobIDIfLive →
	// s.mu.RLock. Pattern mirrors PauseJobByID's pauseCleanup hoist (#537).
	var removeEntryID cronEntryID
	op := func(j *Job) error {
		// Snapshot under s.mu so the rollback restores the exact pre-op
		// view; resumeJobLocked → registerJob mutates entryID +
		// cachedPeriod + cachedSched only after this read so a concurrent
		// reader can never observe a torn write.
		prevEntryID = j.entryID
		prevCachedPeriod = j.cachedPeriod
		prevCachedSched = j.cachedSched // CR-3 (R250531-CR-3): snapshot cachedSched
		prevPaused = j.Paused
		captured = true
		return s.resumeJobLocked(j)
	}
	rollback := func(j *Job) {
		// Only restore if op actually ran and captured the pre-op view —
		// guards against a future refactor that might invoke rollback on
		// the "op never ran" path.
		if !captured {
			return
		}
		// CR-1: capture the freshly-registered entryID for removal OUTSIDE
		// s.mu. Do NOT call s.cron.Remove here — we are under s.mu and
		// cron.Remove sends on an unbuffered channel drained only by the
		// cron-tick goroutine that itself acquires s.mu.RLock → deadlock.
		removeEntryID = j.entryID
		j.entryID = prevEntryID
		j.cachedPeriod = prevCachedPeriod
		j.cachedSched = prevCachedSched // CR-3: restore cachedSched
		j.Paused = prevPaused
	}
	snap, err := s.withJobByIDOpt(id, withJobByIDOpts{
		op:                   op,
		rollbackOnPersistErr: rollback,
	})
	// CR-1: remove the orphaned cron entry now that s.mu is released.
	// removeEntryID is non-zero only when rollback fired (persist failed
	// after op succeeded and registered a new entry). The zero check is
	// defensive; robfig/cron.Remove(0) is a no-op, but being explicit
	// makes the intent clear.
	if removeEntryID != 0 {
		s.cron.Remove(removeEntryID)
	}
	return snap, err
}

// JobUpdate captures fields a dashboard user may edit on an existing cron
// job. Only non-nil pointers are applied, so callers can update a single
// field without resending the rest.
type JobUpdate struct {
	Schedule *string
	Prompt   *string
	WorkDir  *string
	// Notify sets Job.Notify when non-nil. nil leaves the field unchanged;
	// pointer-to-true/false writes the explicit tri-state.
	//
	// R227-CONFIG-1 / R249-CR-15 (#958): use NotifyClear (below) to reset
	// Job.Notify back to legacy-default (nil) once a value has been set.
	// The clear is kept on a separate bool flag rather than overloading
	// Notify with a fourth state so the wire format and existing
	// /api/cron consumers stay source-compatible — a nil Notify still
	// means "leave unchanged", a non-nil Notify still writes the explicit
	// tri-state, and only the additive NotifyClear flag opts into the
	// reset-to-nil behaviour.
	Notify *bool
	// NotifyClear, when set to a pointer-to-true, resets Job.Notify back to
	// nil (legacy-default: inherit the scheduler-wide notify policy). nil or
	// pointer-to-false is a no-op. Applied AFTER Notify so a caller that sets
	// both gets the clear (defensive: the dashboard never sends both, but
	// "clear wins" is the least-surprising precedence — an explicit reset
	// request should not be silently overridden by a stale Notify value in
	// the same patch). R249-CR-15 (#958).
	NotifyClear *bool
	// NotifyPlatform / NotifyChatID behave like Prompt / WorkDir: nil keeps
	// the existing value, a pointer to "" clears it.
	NotifyPlatform *string
	NotifyChatID   *string
	// FreshContext toggles whether each run resets the session before
	// executing. nil leaves existing behavior unchanged.
	FreshContext *bool
	// Title 是人类可读名称。nil 保持原值；pointer 到 "" 会清空
	// （UI 侧回退到 Prompt 首行）。长度由 handler 层先行校验。
	Title *string
	// Backend 是 CLI backend ID（Sprint 6c, docs/rfc/multi-backend.md §9）。
	// nil 保持原值；pointer 到 "" 显式清空，回落到 router default。
	// 字符/长度由 dashboard handler 的 validateCronBackend 先行把关；
	// 未知 backend 不在此处拒绝（router wrapperFor 会 fallback）。
	Backend *string
}

// applyTo writes every non-nil JobUpdate field onto j. R238-ARCH-14
// (#778): the inline `if upd.X != nil { j.X = *upd.X }` ladder used to
// live inside UpdateJob's locked critical section, growing one branch
// per new patchable field. Pulling the dispatch into a method keeps
// UpdateJob's critical section short (one method call instead of a
// 25-line ladder) and gives every patchable field a single edit point —
// new fields land here without touching UpdateJob's body.
//
// Schedule is intentionally NOT applied here. Schedule mutations require
// re-registering the robfig/cron entry under s.mu (with rollback on
// failure) and the helper has no access to *Scheduler. Keeping Schedule
// on the UpdateJob body localises the cron-side coupling and matches
// the issue's "patch model mixes nil-vs-empty" concern only for the
// pure-data fields.
//
// LastSessionID side effect: WorkDir change clears LastSessionID
// because claude JSONL is keyed by cwd. The same caveats from the
// pre-refactor inline comment apply (relies on AddJob/UpdateJob WorkDir
// pre-normalisation; a non-normalised caller risks a spurious clear,
// not data loss).
//
// Caller must hold s.mu (j is the *Job pulled from s.jobs).
func (upd JobUpdate) applyTo(j *Job) {
	if upd.Prompt != nil {
		j.Prompt = *upd.Prompt
	}
	if upd.WorkDir != nil {
		if *upd.WorkDir != j.WorkDir {
			j.LastSessionID = ""
		}
		j.WorkDir = *upd.WorkDir
	}
	if upd.Notify != nil {
		v := *upd.Notify
		j.Notify = &v
	}
	// R249-CR-15 (#958): reset-to-nil opt-in. Applied after Notify so an
	// explicit clear request wins if a caller (incorrectly) sends both.
	if upd.NotifyClear != nil && *upd.NotifyClear {
		j.Notify = nil
	}
	if upd.NotifyPlatform != nil {
		j.NotifyPlatform = *upd.NotifyPlatform
	}
	if upd.NotifyChatID != nil {
		j.NotifyChatID = *upd.NotifyChatID
	}
	if upd.FreshContext != nil {
		j.FreshContext = *upd.FreshContext
	}
	if upd.Title != nil {
		j.Title = *upd.Title
	}
	if upd.Backend != nil {
		j.Backend = *upd.Backend
	}
}

// UpdateJob applies a partial edit to an existing cron job. Schedule changes
// are validated and re-registered atomically (the old robfig entry is
// removed before the new one is installed) so a failed reschedule leaves
// the previous behavior intact. Prompt/WorkDir changes flow through to the
// router stub so the dashboard sidebar reflects the edit immediately.
func (s *Scheduler) UpdateJob(id string, upd JobUpdate) (*Job, error) {
	// Validate schedule first (no lock needed) so we fail fast on bad input.
	if upd.Schedule != nil {
		if *upd.Schedule == "" {
			return nil, fmt.Errorf("schedule must not be empty")
		}
		if err := validateSchedule(*upd.Schedule, s.previewLocation()); err != nil {
			return nil, fmt.Errorf("invalid schedule %q: %w", *upd.Schedule, err)
		}
	}
	// Validate WorkDir against allowedRoot here (lock-free) so dashboard
	// edits fail fast with a clear error instead of silently persisting a
	// path that execute() will later refuse at runtime. AddJob's creation
	// path applies the same check; UpdateJob previously skipped it.
	if upd.WorkDir != nil {
		v := *upd.WorkDir
		if len(v) > MaxWorkDirLen {
			return nil, fmt.Errorf("cron: work_dir too long: %d bytes > %d cap", len(v), MaxWorkDirLen)
		}
		if !utf8.ValidString(v) || containsCronUnsafe(v) {
			return nil, fmt.Errorf("cron: work_dir contains invalid bytes")
		}
		if v != "" && s.allowedRoot != "" {
			if !workDirUnderRoot(v, s.allowedRoot, s.allowedRootResolved) {
				return nil, fmt.Errorf("work_dir outside allowed root")
			}
		}
	}
	if upd.Title != nil {
		if n := utf8.RuneCountInString(*upd.Title); n > MaxCronTitleLen {
			return nil, fmt.Errorf("title too long: %d runes > %d cap", n, MaxCronTitleLen)
		}
	}
	// R244-SEC-P2-5 / #889: UpdateJob is a Scheduler-public entry point that
	// historically wrote *upd.Prompt straight into j.Prompt without a size
	// guard. The dashboard PATCH handler already runs validateCronPrompt at
	// the HTTP edge, but any non-dashboard caller (test, CLI utility, future
	// IM op) bypassing that validator would persist a multi-MB / log-injection
	// prompt to cron_jobs.json. Mirror SetJobPrompt's policy so the cap is
	// consistent across all Scheduler write paths. Empty pointer-to-empty is
	// allowed (clears the prompt to the paused-empty initial state); any
	// non-empty value goes through the strict validator.
	if upd.Prompt != nil && *upd.Prompt != "" {
		if err := ValidatePromptStrict(*upd.Prompt); err != nil {
			return nil, err
		}
	}
	// R171023-SEC-10: UpdateJob is a public Scheduler entry point. The
	// dashboard PATCH handler validates NotifyPlatform/NotifyChatID at the
	// HTTP edge, but a non-dashboard caller (test, CLI, future IM op) can
	// reach UpdateJob directly and bypass those checks. Mirror the same
	// length + UTF-8 + containsCronUnsafe guards that validateJobFields
	// applies on the AddJob path so cron_jobs.json cannot receive oversized
	// or log-injection bytes via this path.
	if upd.NotifyPlatform != nil {
		v := *upd.NotifyPlatform
		if len(v) > MaxNotifyTargetLen {
			return nil, fmt.Errorf("cron: notify_platform too long: %d bytes > %d cap", len(v), MaxNotifyTargetLen)
		}
		if !utf8.ValidString(v) || containsCronUnsafe(v) {
			return nil, fmt.Errorf("cron: notify_platform contains invalid bytes")
		}
	}
	if upd.NotifyChatID != nil {
		v := *upd.NotifyChatID
		if len(v) > MaxNotifyTargetLen {
			return nil, fmt.Errorf("cron: notify_chat_id too long: %d bytes > %d cap", len(v), MaxNotifyTargetLen)
		}
		if !utf8.ValidString(v) || containsCronUnsafe(v) {
			return nil, fmt.Errorf("cron: notify_chat_id contains invalid bytes")
		}
	}
	// R20260603-CR-2: UpdateJob lacked Backend validation. Mirror the same
	// MaxBackendLen + UTF-8 + containsCronUnsafe guards that validateJobFields
	// applies on the AddJob path so cron_jobs.json cannot receive oversized or
	// log-injection bytes via non-dashboard callers (tests, future IM ops).
	if upd.Backend != nil {
		v := *upd.Backend
		if len(v) > MaxBackendLen {
			return nil, fmt.Errorf("cron: backend too long: %d bytes > %d cap", len(v), MaxBackendLen)
		}
		if !utf8.ValidString(v) || containsCronUnsafe(v) {
			return nil, fmt.Errorf("cron: backend contains invalid characters")
		}
	}

	// R239-GO-4: critical section uses defer Unlock so any future return
	// path added inside this block stays correctly unlocked. The closure
	// returns (resultSnapshot, persistCallback, error); save() runs
	// post-unlock to keep the global s.mu off the disk write path.
	//
	// R112714-LOGIC-1: robfig/cron.Remove and .AddFunc both send on
	// unbuffered channels that are drained by the cron run goroutine.
	// Calling them while holding s.mu can cause a lock-order inversion:
	// a tick callback goroutine (spawned by the run loop) calls
	// executeJobIDIfLive → s.mu.RLock, and if the run loop is mid-select
	// processing the timer case it cannot drain the Remove/Add channel
	// while we hold the write lock. Hoist all cron channel operations to
	// after the IIFE (i.e. after s.mu is released), mirroring the pattern
	// established by PauseJobByID (#537) and ResumeJobByID (#1226).
	//
	// The IIFE now only: applies non-schedule fields, snapshots the old
	// cron entry ID (clearing j.entryID=0 under lock), updates j.Schedule,
	// and persists. The Remove + AddFunc calls happen post-unlock.
	// entryID is not persisted (runtime-only field) so persisting with
	// entryID=0 is safe — the post-unlock registerJob writes it back.
	var (
		schedRemoveEntryID cronEntryID
		schedOldSchedule   string
		schedNewSchedule   string
		schedNeedsRereg    bool
	)
	result, save, err := func() (Job, func(), error) {
		s.mu.Lock()
		defer s.mu.Unlock()

		j, ok := s.jobs[id]
		if !ok {
			return Job{}, nil, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
		}

		// R238-ARCH-14 (#778): non-Schedule fields applied via JobUpdate.applyTo
		// so the locked critical section stays short and adding a new patchable
		// field is a single edit point on the helper. Schedule stays inline
		// because it requires re-registering the robfig/cron entry under s.mu
		// with rollback semantics (helper has no *Scheduler access).
		//
		// R20260603140013-CR-1: snapshot the live Job by value BEFORE applyTo
		// (and before the Schedule mutation below) so a persistJobsLocked
		// failure can restore the in-memory job to its pre-update state. Without
		// this, applyTo's writes (Prompt/WorkDir/Notify/NotifyPlatform/
		// NotifyChatID/FreshContext/Title/Backend/LastSessionID) plus any
		// Schedule field change stay in *j while disk keeps the old values —
		// a restart replays the stale persisted job and silently reverts the
		// edit, diverging memory from disk. The value copy captures the Notify
		// *bool pointer too; applyTo reassigns j.Notify to a fresh &v, so
		// restoring the old pointer correctly drops the would-be new value.
		// This mirrors the Schedule rollbackOnPersistErr intent for the
		// non-Schedule fields. entryID/cachedSched are runtime-only and are
		// also captured, which is harmless: on this error path we abort before
		// any re-registration, so the snapshot's pre-update runtime fields are
		// the correct ones to keep.
		preUpdate := *j
		upd.applyTo(j)

		if upd.Schedule != nil && *upd.Schedule != j.Schedule {
			// R236-QA-08: snapshot the old schedule for rollback.
			// R112714-LOGIC-1: instead of calling s.cron.Remove + s.registerJob
			// inside the lock, snapshot the entry ID to remove and clear
			// j.entryID=0 here. The actual Remove + AddFunc (registerJob) happen
			// post-unlock below. entryID is runtime-only (not persisted), so
			// persisting with entryID=0 is safe.
			schedOldSchedule = j.Schedule
			schedNewSchedule = *upd.Schedule
			j.Schedule = schedNewSchedule
			if !j.Paused {
				// Capture entry to remove; clear under lock so concurrent
				// readers see entryID=0 immediately (NextRun will be zero
				// briefly until registerJob runs post-unlock).
				schedRemoveEntryID = j.entryID
				j.entryID = 0
				j.cachedPeriod = 0
				j.cachedSched = nil
				schedNeedsRereg = true
			}
		}

		save, perr := s.persistJobsLocked()
		if perr != nil {
			// R20260603140013-CR-1: persist failed — restore the live job to its
			// pre-update snapshot so memory matches the (unchanged) disk state.
			// Done under the same lock before unlocking so no reader observes the
			// half-applied edit. schedNeedsRereg is left as captured but the
			// post-unlock re-registration block only runs after this IIFE returns
			// nil error; on perr the caller returns immediately at the err!=nil
			// guard, so the re-reg block is never reached.
			*j = preUpdate
			return Job{}, nil, perr
		}
		// Value-copy while still under lock so the caller sees a stable result
		// even if another goroutine mutates the job right after we unlock.
		return *j, save, perr
	}()
	if err != nil {
		return nil, err
	}
	// R112714-LOGIC-1: all cron channel operations happen here, after s.mu
	// is released. Remove the old entry first, then register the new one.
	// registerJob itself calls s.cron.AddFunc (c.add channel send) and
	// s.cron.Entry (c.snapshot channel send) — both must be outside s.mu.
	// The entryID write-back re-acquires s.mu briefly.
	if schedNeedsRereg {
		if schedRemoveEntryID != 0 {
			s.cron.Remove(schedRemoveEntryID)
		}
		// Register the new schedule entry. On failure, roll back the in-memory
		// Schedule field and persist state, then best-effort re-register the
		// old schedule so NextRun stays populated (R246-GO-10).
		s.mu.Lock()
		j := s.jobs[id]
		var schedRegErr error
		if j != nil {
			prevCachedSched := j.cachedSched // R20260602-CR-1: snapshot before registerJob mutates it
			schedRegErr = s.registerJob(j)
			if schedRegErr != nil {
				// Rollback in-memory Schedule so subsequent reads reflect the
				// pre-update state.
				j.Schedule = schedOldSchedule
				j.entryID = 0
				j.cachedPeriod = 0
				j.cachedSched = prevCachedSched // R20260602-CR-1: restore, not nil
			}
		}
		s.mu.Unlock()
		if schedRegErr != nil {
			// Best-effort re-register old schedule outside lock (R112714-LOGIC-1).
			s.mu.Lock()
			if j2 := s.jobs[id]; j2 != nil {
				if reErr := s.registerJob(j2); reErr != nil {
					slog.Error("cron: failed to restore previous schedule after UpdateJob rollback",
						"job_id", id, "schedule", schedOldSchedule, "err", reErr)
				}
				// Re-persist with the rolled-back schedule so disk stays
				// consistent with in-memory state.
				if save2, perr2 := s.persistJobsLocked(); perr2 == nil {
					s.mu.Unlock()
					save2()
				} else {
					s.mu.Unlock()
					slog.Error("cron: re-persist after UpdateJob rollback failed",
						"job_id", id, "err", perr2)
				}
			} else {
				s.mu.Unlock()
			}
			return nil, fmt.Errorf("re-register cron: %w", schedRegErr)
		}
	}
	// R20260604-CR-05: refresh LastSessionID from live job after schedNeedsRereg
	// re-registration. The IIFE snapshotted result = *j before registerJob ran
	// (line 1329); a concurrent recordTerminalResult may have written a newer
	// j.LastSessionID between that snapshot and here, causing registerStubFromJob
	// to anchor the sidebar on a stale session. Only the rereg path needs this
	// refresh — on the non-rereg path result is the latest snapshot.
	if schedNeedsRereg {
		s.mu.RLock()
		if lj := s.jobs[id]; lj != nil {
			result.LastSessionID = lj.LastSessionID
		}
		s.mu.RUnlock()
	}
	save()
	// Pass the snapshotted value (via result) to registerStub so a concurrent
	// SetJobPrompt cannot tear the Prompt/WorkDir pointers we read.
	s.registerStubFromJob(&result)
	slog.Info("cron job updated", "job_id", id,
		"schedule_changed", upd.Schedule != nil,
		"prompt_changed", upd.Prompt != nil,
		"workdir_changed", upd.WorkDir != nil,
		"fresh_context_changed", upd.FreshContext != nil)
	return &result, nil
}

// SetJobPrompt sets a job's FIRST prompt. If the job was paused with an empty
// prompt (created from dashboard), it also unpauses and registers the schedule.
//
// Contract: this is an auto-fill-once operation, NOT a general update. If the
// job already has a non-empty prompt it returns ErrPromptAlreadySet and does
// not mutate anything — callers that want to CHANGE an existing prompt must use
// UpdateJob. The sentinel replaces the previous silent `return nil` (#1503) so
// the no-op is observable; IM auto-save paths treat ErrPromptAlreadySet as
// benign, HTTP/dashboard callers may map it to 409 Conflict.
//
// Both IM (Hub.runTurn / runTurnPassthrough) and dashboard wshub paths land
// here. The dashboard already validates via server.validateCronPrompt at the
// HTTP edge, but the IM path historically only rejected the empty string —
// so a crafted IM payload could persist multi-MB / bidi / log-injection
// bytes into cron_jobs.json. Centralising the policy in
// ValidatePromptStrict keeps IM and dashboard surfaces in lockstep
// (R243-SEC-8 REPEAT-5). Callers should errors.Is(err, ErrInvalidPrompt)
// to distinguish input-validation failures from ErrJobNotFound /
// ErrPersistFailed.
func (s *Scheduler) SetJobPrompt(id, prompt string) error {
	if err := ValidatePromptStrict(prompt); err != nil {
		return err
	}
	// R246-SEC-10: bound prompt size on this dashboard write path. The
	// dashboard handler runs validateCronPrompt (which enforces
	// maxCronPromptBytesDashboard == cron.MaxPromptBytes) before reaching
	// here, but SetJobPrompt is also exposed via Scheduler so any future
	// caller (or a code path that bypasses validateCronPrompt) would write
	// an unbounded prompt to disk and amplify it across LastResult records.
	// Mirror the same cap as cron run prompts.
	if len(prompt) > MaxPromptBytes {
		return fmt.Errorf("prompt too large: %d bytes (cap %d)", len(prompt), MaxPromptBytes)
	}

	// R112714-LOGIC-2: the previous code used s.mu.Lock() without defer,
	// relying on 5 explicit Unlock() calls across all return paths. A panic
	// inside resumeJobLocked (→ registerJob → AddFunc) would skip every
	// explicit Unlock, permanently locking the mutex. Wrap the critical
	// section in an IIFE with defer s.mu.Unlock() so the lock is always
	// released regardless of panics. The IIFE returns (save, pauseCleanup,
	// stubFields, err); save() and pauseCleanup() run post-unlock so the
	// cron.Remove channel send (pauseCleanup) stays outside s.mu.
	// All early-return semantics are preserved via the IIFE's return values.
	type stubFields struct {
		workDir     string
		lastSession string
	}
	save, pauseRollbackCleanup, stub, err := func() (func(), func(), stubFields, error) {
		s.mu.Lock()
		defer s.mu.Unlock()

		j, ok := s.jobs[id]
		if !ok {
			return nil, nil, stubFields{}, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
		}
		if j.Prompt != "" {
			// R250531-CR-8 (#1503): the prompt is already set. SetJobPrompt only
			// auto-fills the first prompt; it never overwrites. Previously this
			// silently returned nil (200 OK, no change), which misled any caller
			// trying to edit an existing prompt. Return a sentinel so the no-op is
			// observable — IM auto-save callers treat it as benign, dashboard /
			// API callers can map it to 409 and route real edits through UpdateJob.
			return nil, nil, stubFields{}, ErrPromptAlreadySet
		}

		j.Prompt = prompt
		// R246-CR-247: capture identity fields under lock so the stub refresh
		// below reads stable values even if a concurrent UpdateJob mutates *Job
		// after the IIFE's deferred Unlock fires. Mirrors AddJob / UpdateJob.
		sf := stubFields{workDir: j.WorkDir, lastSession: j.LastSessionID}
		waspaused := j.Paused
		if j.Paused {
			// Delegate unpause to the shared helper so the registerJob + Paused
			// flag transition stays consistent with PauseJob/ResumeJob/UpdateJob
			// paths. R226-CR-16.
			if err := s.resumeJobLocked(j); err != nil {
				j.Prompt = "" // rollback: Prompt was empty before this call
				return nil, nil, stubFields{}, err
			}
		}
		saveFn, perr := s.persistJobsLocked()
		if perr != nil {
			// Rollback in-memory state before releasing the lock so the
			// live view never reflects an un-persisted mutation.
			// pauseJobLocked failure here is best-effort: only logged, never
			// suppresses the original perr returned to the caller. R243-GO-5.
			// R236-QA-03 (#537): pauseJobLocked now returns a cron.Remove
			// closure to be invoked AFTER s.mu is released. We discard
			// pauseRollbackCleanup if the caller was already in a "no entry"
			// state (e.g. paused with entryID==0), but always invoke it
			// post-Unlock so the unbuffered c.remove channel send doesn't
			// happen while we still hold the scheduler mutex.
			j.Prompt = ""
			var cleanupFn func()
			if waspaused && !j.Paused {
				c, rbErr := s.pauseJobLocked(j)
				if rbErr != nil && !errors.Is(rbErr, ErrJobAlreadyPaused) {
					slog.Warn("cron rollback after persist failure also failed",
						"job_id", j.ID, "rollback_err", rbErr, "orig_err", perr)
				}
				cleanupFn = c
			}
			return nil, cleanupFn, stubFields{}, perr
		}
		return saveFn, nil, sf, nil
	}()

	// Run cron.Remove cleanup outside s.mu — pauseRollbackCleanup sends on
	// the unbuffered c.remove channel (R236-QA-03 / #537).
	if pauseRollbackCleanup != nil {
		pauseRollbackCleanup()
	}
	if err != nil {
		return err
	}
	save()
	// R246-CR-247: refresh the router stub so the dashboard sidebar
	// immediately reflects the new prompt. Without this, the stub keeps the
	// empty-prompt state from the initial AddJob until the next executeJob
	// tick rebuilds it.
	s.registerStubByValue(id, stub.workDir, prompt, stub.lastSession)
	slog.Info("cron job prompt set", "job_id", id, "prompt_len", len(prompt))
	return nil
}

// withJobByPrefix is the IM-prefix counterpart to withJobByID. It collapses
// DeleteJob / PauseJob / ResumeJob (R238-ARCH-4 / #743) — three ~25-line
// twins of "lock → findByPrefix → mutate → persist → unlock → side-effect"
// — into a single 3-phase frame. Layout mirrors withJobByID exactly so a
// reader who learns one helper has learned both.
//
//  1. Acquire s.mu, look up by (idPrefix, plat, chatID); a miss surfaces
//     the findByPrefixLocked error verbatim (typically ErrJobNotFound or
//     "ambiguous prefix").
//  2. Run op(j) inside s.mu; an op error skips persist + postCleanup.
//  3. Release s.mu, run postCleanup(j) lock-free (router.Reset /
//     runStore.DeleteJob), then call save() to land the persist.
//
// Lock-order rationale follows withJobByID's: postCleanup must NOT run
// under s.mu because router callbacks may re-take it (notifyChange
// dead-locks otherwise — R240-GO-1). save() runs after postCleanup so a
// persist failure leaves the in-memory + side-effect state already
// committed (matches the pre-refactor semantics that runStore.DeleteJob
// fires even when persist fails — R238-GO-3).
//
// rollbackOnPersistErr (optional, pass nil for DeleteJob callers that do not
// need it): if non-nil and persistJobsLocked returns an error, it is called
// under s.mu BEFORE the snapshot copy — restoring *j to its pre-op state so
// disk (un-persisted) and memory stay aligned. postCleanup is skipped on
// rollback (mirrors withJobByIDOpt's contract — R20260527-COR-1 / #1272).
//
// Error precedence (preserved from the originals):
//   - find miss      → (nil, find err)
//   - op error       → (nil, op err)        ; persist + postCleanup skipped
//   - persist error  → (nil, persist err)   ; postCleanup ALREADY ran
//     (unless rollbackOnPersistErr is set — then postCleanup is skipped)
//   - success        → (*Job, nil)
//
// withJobByPrefixOpts bundles the optional knobs withJobByPrefix accepts,
// mirroring withJobByIDOpts so the IM-prefix path stays symmetric with the
// exact-ID path. A struct (instead of a variadic) gives compile-time
// protection: callers cannot silently pass more than one rollback hook.
//
//   - rollbackOnPersistErr: in-lock undo of the op's mutation when
//     persistJobsLocked fails. When non-nil and persist fails, this restores
//     *j BEFORE the snapshot copy and skips postCleanup so the caller observes
//     "no change applied". nil for callers (DeleteJob) that do not need it.
type withJobByPrefixOpts struct {
	rollbackOnPersistErr jobSideEffect // R249-ARCH-20 (#985)
}

// withJobByPrefixResult bundles the locked-section outputs of
// withJobByPrefix so the post-unlock flow reads as named-field branches
// rather than five sibling vars mutated inside an IIFE — the prefix-path
// twin of withJobByIDResult. R249-CR-7 (#951).
type withJobByPrefixResult struct {
	save       func()
	snapshot   Job
	findErr    error
	opErr      error
	perr       error
	rolledBack bool
}

// lockedJobPrefixOp runs the find-by-prefix + op + persist + (optional)
// rollback steps for withJobByPrefix entirely under s.mu, mirroring
// lockedJobOp on the by-ID path. Splitting it out of the IIFE keeps every
// s.mu-guarded mutation in one named scope.
func (s *Scheduler) lockedJobPrefixOp(idPrefix, plat, chatID string, op func(j *Job) error, rollback func(j *Job)) withJobByPrefixResult {
	var r withJobByPrefixResult
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.findByPrefixLocked(idPrefix, plat, chatID)
	if err != nil {
		r.findErr = err
		return r
	}
	if op != nil {
		if err := op(j); err != nil {
			r.opErr = err
			return r
		}
	}
	r.save, r.perr = s.persistJobsLocked()
	// R20260531070014-CR-1/CR-2: mirror withJobByIDOpt's rollback contract —
	// if persistJobsLocked failed after op mutated *j, restore in-memory state
	// under s.mu so disk (un-persisted) and memory stay aligned.
	if r.perr != nil && rollback != nil {
		rollback(j)
		r.rolledBack = true
	}
	// R242-GO-3 mirror (#548): value-copy under s.mu so postCleanup and
	// the caller read a stable Job even if a concurrent UpdateJob /
	// SetJobPrompt mutates the live *j right after Unlock. Matches
	// withJobByIDOpt's "snapshot = *j" pattern. [R250531-CR-2]
	r.snapshot = *j
	return r
}

func (s *Scheduler) withJobByPrefix(
	idPrefix, plat, chatID string,
	op lockedJobOp,
	postCleanup jobSideEffect,
	opts withJobByPrefixOpts,
) (*Job, error) {
	r := s.lockedJobPrefixOp(idPrefix, plat, chatID, op, opts.rollbackOnPersistErr)
	save, snapshot, perr, rolledBack := r.save, r.snapshot, r.perr, r.rolledBack

	if r.findErr != nil {
		return nil, r.findErr
	}
	if r.opErr != nil {
		return nil, r.opErr
	}
	// R20260531070014-CR-1/CR-2: on rollback skip postCleanup (mirrors
	// withJobByIDOpt — the cron.Remove hoist must not fire when the in-memory
	// mutation was reversed). Return perr so the caller surfaces the persist
	// failure as a 5xx and the operator can retry.
	if rolledBack {
		return nil, perr
	}
	if postCleanup != nil {
		postCleanup(&snapshot)
	}
	if perr != nil {
		return nil, perr
	}
	save()
	return &snapshot, nil
}

// DeleteJob removes a job by ID prefix (scoped to the given chat).
func (s *Scheduler) DeleteJob(idPrefix, plat, chatID string) (*Job, error) {
	// R20260605B-CORR-6 (#1810): mirror DeleteJobByID — deleteJobLocked
	// snapshots the cron entryID under s.mu and postCleanup runs s.cron.Remove
	// after the lock is released.
	var removeEntryID cronEntryID
	return s.withJobByPrefix(
		idPrefix, plat, chatID,
		func(j *Job) error {
			removeEntryID = s.deleteJobLocked(j)
			return nil
		},
		// R244-ARCH-13 (#1053): the IM-prefix DeleteJob path is the cron
		// alias side of the same lifecycle as DeleteJobByID, so both share
		// the deleteJobPostCleanup helper rather than open-coding the
		// cron.Remove + router.Reset + runStore.DeleteJob + runningJobs-reclaim
		// sequence twice (R240-GO-1 / R238-GO-3 / R242-ARCH-15 all documented there).
		func(j *Job) { s.deleteJobPostCleanup(j.ID, removeEntryID) },
		withJobByPrefixOpts{},
	)
}

// PauseJob pauses a job by ID prefix.
//
// R236-QA-03 (#537): same lock-order pattern as PauseJobByID — the
// cron.Remove returned by pauseJobLocked runs in postCleanup so the
// unbuffered c.remove channel send doesn't happen under s.mu.
//
// R20260531070014-CR-1: mirror PauseJobByID's rollbackOnPersistErr contract
// (#1272). If persistJobsLocked fails after pauseJobLocked already mutated
// (j.entryID=0, j.Paused=true), restore the pre-op (entryID, Paused) under
// s.mu so disk (un-persisted Paused=false) and memory stay aligned — preventing
// the "ghost-paused job that never fires" split-brain on restart.
// postCleanup (cron.Remove hoist) is skipped on rollback so the cron entry
// stays alive and the next tick still fires the now-active job.
func (s *Scheduler) PauseJob(idPrefix, plat, chatID string) (*Job, error) {
	var pauseCleanup func()
	var prevEntryID cronEntryID
	var prevPaused bool
	var captured bool
	op := func(j *Job) error {
		// Snapshot under s.mu so the rollback restores the exact
		// pre-op view; pauseLocked mutates j.entryID + j.Paused only
		// after this read.
		prevEntryID = j.entryID
		prevPaused = j.Paused
		captured = true
		c, err := s.pauseJobLocked(j)
		pauseCleanup = c
		return err
	}
	postCleanup := func(_ *Job) {
		if pauseCleanup != nil {
			pauseCleanup()
		}
	}
	rollback := func(j *Job) {
		// Only restore if op actually ran and captured the pre-op view.
		if !captured {
			return
		}
		j.entryID = prevEntryID
		j.Paused = prevPaused
		// Drop pauseCleanup so no future code path accidentally fires
		// the cron.Remove that we are choosing NOT to run (the entry
		// must stay alive since the pause was not persisted).
		pauseCleanup = nil
	}
	return s.withJobByPrefix(idPrefix, plat, chatID, op, postCleanup, withJobByPrefixOpts{
		rollbackOnPersistErr: rollback,
	})
}

// ResumeJob resumes a paused job by ID prefix.
//
// R20260531070014-CR-2: mirror ResumeJobByID's rollbackOnPersistErr contract
// (#1226). resumeJobLocked → registerJob mutates j.entryID + j.cachedPeriod +
// j.cachedSched and then flips j.Paused=false BEFORE persistJobsLocked runs.
// A persist failure after that op-success path would leave in-memory state with
// a live cron entry + Paused=false while disk still shows Paused=true — on
// restart the scheduler re-registers the schedule on top of the surviving
// runtime entry, producing a double-fire. Capture the pre-op state under s.mu
// and install a rollback that removes the cron entry and restores
// (entryID, cachedPeriod, cachedSched, Paused) so in-memory matches disk.
//
// Lock-order: rollback runs under s.mu; calling s.cron.Remove there would
// send on the unbuffered c.remove channel drained only by the cron-tick
// goroutine, which itself calls s.mu.RLock → deadlock. Mirror ResumeJobByID's
// removeEntryID pattern: capture the freshly-registered entryID inside the
// rollback closure and call s.cron.Remove AFTER withJobByPrefix returns
// (s.mu released).
func (s *Scheduler) ResumeJob(idPrefix, plat, chatID string) (*Job, error) {
	var prevEntryID cronEntryID
	var prevCachedPeriod time.Duration
	var prevCachedSched robfigcron.Schedule
	var prevPaused bool
	var captured bool
	// removeEntryID is non-zero only when rollback fired; cron.Remove must
	// be called after withJobByPrefix returns (s.mu released) to avoid
	// the lock-order inversion described above.
	var removeEntryID cronEntryID
	op := func(j *Job) error {
		// Snapshot under s.mu so the rollback restores the exact pre-op
		// view; resumeJobLocked → registerJob mutates entryID +
		// cachedPeriod + cachedSched only after this read.
		prevEntryID = j.entryID
		prevCachedPeriod = j.cachedPeriod
		prevCachedSched = j.cachedSched
		prevPaused = j.Paused
		captured = true
		return s.resumeJobLocked(j)
	}
	rollback := func(j *Job) {
		// Only restore if op actually ran and captured the pre-op view.
		if !captured {
			return
		}
		// Capture the freshly-registered entryID for removal OUTSIDE s.mu.
		// Do NOT call s.cron.Remove here — we are under s.mu and cron.Remove
		// sends on an unbuffered channel drained only by the cron-tick goroutine
		// that itself acquires s.mu.RLock → deadlock.
		removeEntryID = j.entryID
		j.entryID = prevEntryID
		j.cachedPeriod = prevCachedPeriod
		j.cachedSched = prevCachedSched
		j.Paused = prevPaused
	}
	snap, err := s.withJobByPrefix(idPrefix, plat, chatID, op, nil, withJobByPrefixOpts{
		rollbackOnPersistErr: rollback,
	})
	// Remove the orphaned cron entry now that s.mu is released. removeEntryID
	// is non-zero only when rollback fired (persist failed after op succeeded
	// and registered a new entry). robfig/cron.Remove(0) is a no-op, but being
	// explicit about the guard makes the intent clear.
	if removeEntryID != 0 {
		s.cron.Remove(removeEntryID)
	}
	return snap, err
}

// NextRun returns the next scheduled run time for a job. R247-GO-9
// [REPEAT-2]: the prior implementation read j.entryID under s.mu.RLock
// then released the lock before calling s.cron.Entry(entryID). A
// concurrent UpdateJob path (which Remove+AddFunc the entry under s.mu)
// could race in that window and return the cron-library zero-value
// Entry{} (Next == time.Time{}) for what is in fact a still-scheduled
// job. Same root cause as R246-GO-1 on TriggerNow's entry read.
//
// Hold s.mu.RLock across both the entryID load AND the cron.Entry call
// so the entry the caller asked about cannot be removed mid-read.
// robfig/cron.Cron.Entry takes its own internal lock — there is no
// lock-order conflict with s.mu (cron's locks never call back into
// scheduler code), so the cross-call hold is safe. The cost is one
// extra contended RLock window per dashboard 1Hz poll, dwarfed by
// the s.cron.Entry sort+scan it wraps.
//
// R238-ARCH-17 (#784): entryID is an unexported runtime-only field that
// is zero-valued on any *Job that did not flow through this Scheduler's
// AddJob / loadJobs path (e.g. a test fixture, a deserialised snapshot,
// or a cross-package caller that passed json.Unmarshal output). The
// previous implementation silently returned time.Time{} in that case,
// which the dashboard / IM reply layer renders as "01/01 00:00" — a
// misleading "unknown next run" that looks like a real schedule. When
// j.entryID is zero, fall back to looking up the live *Job by j.ID in
// s.jobs and reading its entryID; the on-record entryID is the source
// of truth, and a non-existent jobID yields a true zero return.
func (s *Scheduler) NextRun(j *Job) time.Time {
	if j == nil {
		return time.Time{}
	}
	// R250-PERF-14-adjacent lock-order fix (#1117): resolve entryID under
	// s.mu.RLock, then RELEASE s.mu before calling s.cron.Entry(). Entry()
	// is implemented as `for _, e := range c.Entries()` and Entries()
	// round-trips through the dispatcher's snapshot channel guarded by
	// robfig/cron's runningMu. Holding s.mu across that call inverts the
	// lock order the cron dispatch path takes (cron-internal → execute →
	// recordResult → s.mu.Lock) — the exact discipline
	// ListAllJobsWithNextRun's godoc documents and follows. NextRun was a
	// straggler still calling cron.Entry inside s.mu; aligning it with the
	// release-before-Entries discipline removes the lock-ordering
	// inconsistency without changing the observable result (entryID is the
	// source-of-truth snapshot; a concurrent Remove that lands after the
	// unlock yields a zero Entry.Next, the same "no live schedule" answer
	// the in-lock path produced). TriggerNow intentionally keeps its
	// cross-lock (see R250-GO-2 there) because it needs a single consistent
	// instant for the entry-gone check against a racing DeleteJob; NextRun
	// only reads Entry.Next so it has no such consistency requirement.
	s.mu.RLock()
	entryID := j.entryID
	if entryID == 0 && j.ID != "" {
		if live, ok := s.jobs[j.ID]; ok {
			entryID = live.entryID
		}
	}
	s.mu.RUnlock()
	if entryID == 0 {
		return time.Time{}
	}
	entry := s.cron.Entry(entryID)
	return entry.Next
}

// cronEntryGoneLocked reports whether the robfig/cron Entry identified by id
// has been removed (or never existed). robfig/cron's Entry(id) returns a
// zero Entry struct when the entry is unknown, distinguishable by
// WrappedJob == nil — but consumers that test that field directly leak
// the lib's internal struct shape into business code. This helper is the
// single point at which scheduler code touches robfig/cron's removed-entry
// sentinel; any future lib bump that changes the sentinel (or replaces
// Entry() with HasEntry / Lookup-style API) lands here once.
//
// Caller must hold s.mu.RLock or s.mu.Lock — concurrent DeleteJob calls
// s.cron.Remove under s.mu.Lock, so reading the entry without a scheduler
// lock can race with removal. The current caller (TriggerNow) already
// holds the lock for its own snapshotting reasons; the helper does not
// re-acquire so it can be used inside an existing lock window without
// lock-order surprises.
//
// R242-ARCH-29 (#774).
func (s *Scheduler) cronEntryGoneLocked(id cronEntryID) bool {
	if id == 0 {
		return true
	}
	return s.cron.Entry(id).WrappedJob == nil
}

// TriggerNow manually executes a job by ID in a new goroutine (for debugging/dashboard).
// Returns an error if the job is not found, paused, or has no prompt.
func (s *Scheduler) TriggerNow(id string) error {
	s.mu.RLock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.RUnlock()
		return fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	if j.Paused {
		s.mu.RUnlock()
		return fmt.Errorf("%w: id %q", ErrJobPaused, id)
	}
	if j.Prompt == "" {
		s.mu.RUnlock()
		return fmt.Errorf("%w: id %q", ErrJobNoPrompt, id)
	}
	entryID := j.entryID
	jobID := j.ID
	// Register the trigger goroutine with triggerWG before releasing s.mu.
	// This prevents a Stop() on another goroutine from observing triggerWG as
	// empty and returning before our goroutine starts. We pair Add(1) here
	// with the single deferred Done() in the spawned goroutine body below.
	s.triggerWG.Add(1)

	// R250-GO-2: hold s.mu.RLock across s.cron.Entry(entryID) and the
	// WrappedJob nil check so a concurrent DeleteJob (which calls
	// s.cron.Remove under s.mu.Lock) cannot observe entryID-in-flight
	// while we're mid-lookup. cron's internal lock cannot call back into
	// scheduler code, so cross-lock holding is safe here. (NextRun no
	// longer cross-locks — #1117 moved its cron.Entry read outside s.mu to
	// align with ListAllJobsWithNextRun's release-before-Entries
	// discipline; TriggerNow keeps the cross-lock because it needs the
	// entry-gone check and the s.mu RLock to observe a single consistent
	// instant against a racing DeleteJob.)
	//
	// TriggerNow 不再通过 cron chain 的 WrappedJob.Run()——因为我们要跳过
	// jitter（用户显式 "run now" 期望立刻跑）。改为直接 executeOpt(..., true)。
	// 去 chain 后失去的保护：
	//   1) SkipIfStillRunning —— executeOpt 内部的 jobRunningGuard CAS
	//      同样拒绝重叠，等效覆盖。
	//   2) Recover（panic） —— execute 自身走 session.Send，session 层
	//      panic 已经被上层 recover；即便有残留 panic 也只影响此 goroutine，
	//      不会污染 robfig/cron 调度器。
	// 但必须保留"entry 已被并发 DeleteJob 清掉"的分支：此时 cron.Entry()
	// 的 WrappedJob 为 nil，我们应该把这当作"entry gone"静默退出，不再
	// 走 executeOpt（可能引用已被清理的 session router / job 指针）。
	// 相关测试：TestTriggerNow_EntryGoneReleasesWG（trigger_now_wg_done_test.go）。
	// R192-CRON-B: cron-v2-polish §3.2 jitter。
	// R242-ARCH-29 (#774): route the WrappedJob == nil sentinel through
	// cronEntryGoneLocked so the robfig/cron internal-struct shape stays
	// behind one helper — a future lib bump that switches to a
	// HasEntry / Lookup API lands once.
	//
	// R247-CR-29 (#596): the entry-gone check is resolved here (under RLock,
	// for the single-consistent-instant race guard) and reduced to one bool
	// so the function spawns exactly ONE goroutine with one `defer Done()`.
	// entryID==0 means the job is paused / unregistered — never "gone", so it
	// proceeds to executeIfNotDeletedOrPaused which re-checks live state.
	entryGone := entryID != 0 && s.cronEntryGoneLocked(entryID)
	s.mu.RUnlock()

	go func() {
		defer s.triggerWG.Done()
		if entryGone {
			slog.Debug("TriggerNow: cron entry gone (concurrent delete?)", "job_id", id, "entry_id", entryID)
			return
		}
		s.executeIfNotDeletedOrPaused(jobID)
	}()
	return nil
}

// registerJob registers a job with the robfig/cron scheduler.
//
// The closure captures the job's ID rather than the *Job pointer: if the
// job is removed and re-added (UpdateJob path) while the scheduler goroutine
// holds an old entry, we want the next tick to resolve the currently-registered
// job rather than fire against a stale pointer whose fields may have diverged
// from the user's intent.
//
// R247-CR-10: tick-dispatch closure routes through executeJobIDIfLive
// shared with TriggerNow's executeIfNotDeletedOrPaused, so the
// deleted/paused pre-flight gate stays in one place. A Pause that lands
// between cron-tick dispatch and our re-lock is honored — PauseJobByID
// removes the entry via cron.Remove(), so normally this tick wouldn't
// fire, but robfig/cron may already be mid-dispatch when Remove runs,
// yielding exactly this race.
func (s *Scheduler) registerJob(j *Job) error {
	jobID := j.ID
	// R247-CR-10 / R250-CR-1 (#1134): route the scheduled tick through
	// executeJobIDIfLive so the {RLock → exists/paused → executeOpt}
	// sequence shared with TriggerNow lives in one place. The closure
	// captures jobID (not *Job) so an UpdateJob remove+re-add between
	// tick dispatch and re-lock resolves to the freshest pointer. The
	// "tick fired for job paused concurrently" race (PauseJobByID's
	// cron.Remove vs robfig mid-dispatch) is honoured by
	// executeJobIDIfLive's paused branch — same Debug log, same skip.
	// The previous godoc named "executeIfReadyOpt", a rename casualty
	// from R247-CR-10 that no helper actually carries.
	//
	// R246-ARCH-9 (#785): the AddFunc closure is constructed via
	// (*Scheduler).newCronTickCallback so the dispatch-boundary contract
	// (jobID-only capture, no *Job pointer leak, single executeJobIDIfLive
	// call site) is documented and pinned in one place. The Scheduler's
	// stopCtx struct field still owns the lifecycle context — robfig/cron's
	// AddFunc takes a func() with no ctx parameter so the field cannot be
	// eliminated entirely until the upstream API grows ctx-aware Schedule.
	// Wrapping the closure here at least makes the dispatch boundary
	// explicit for future ctx-flow refactors (e.g. lifting executeOpt's
	// downstream s.stopCtx reads to receive ctx as a parameter).
	entryID, err := s.cron.AddFunc(j.Schedule, s.newCronTickCallback(jobID))
	if err != nil {
		return fmt.Errorf("register cron: %w", err)
	}
	j.entryID = entryID
	// R242-PERF-2 (#664): cache the schedule period now so the per-tick
	// applyJitterSched fast-path can read it instead of running 2× sched.Next
	// on every fire. Period only depends on Schedule; UpdateJob's Schedule
	// branch (line ~627) calls registerJob again after Remove, so the cache
	// stays in lockstep with the live entry. Zero on parse failure leaves
	// callers on the existing fallback (jitterSleep with period<=0 uses the
	// full jitterMax window).
	if sched := s.cron.Entry(entryID).Schedule; sched != nil {
		j.cachedPeriod = schedulePeriodFromSched(sched, time.Now())
		// R241-PERF-3 (#477): stash the parsed schedule alongside cachedPeriod
		// so dashboard handleList's HasMissedSchedule fanout (1Hz × N jobs)
		// can call HasMissedScheduleCached instead of cronParser.Parse on
		// every tick. Lifetime mirrors cachedPeriod — UpdateJob's Schedule
		// branch calls registerJob again so the cache stays in lockstep.
		j.cachedSched = sched
	} else {
		j.cachedPeriod = 0
		j.cachedSched = nil
	}
	return nil
}

// newCronTickCallback returns the func() closure registered with
// robfig/cron's AddFunc for jobID. R246-ARCH-9 (#785): isolating the
// dispatch boundary in one factory makes three contracts explicit:
//
//  1. The closure captures jobID by value, NOT a *Job pointer. An
//     UpdateJob remove+re-add between tick dispatch and re-lock must
//     resolve to the freshest entry, which executeJobIDIfLive does by
//     re-reading s.jobs[jobID] under RLock. Capturing *Job here would
//     leak a stale pointer past the next UpdateJob.
//
//  2. The closure delegates to executeJobIDIfLive — never calls
//     executeOpt directly — so the deleted/paused pre-flight gate
//     stays shared with TriggerNow's path. R247-CR-10 / R250-CR-1
//     (#1134) is the historical anchor.
//
//  3. The viaTriggerNow=false / logSubject="cron" pair is fixed at
//     the dispatch boundary; future tick-dispatch fan-outs (e.g. a
//     "missed-schedule replay" trigger) must mint their own factory
//     to keep the trigger-source label in lockstep with the dispatch
//     path.
//
// Lifting this from an inline closure also gives a stable structural
// anchor for future ctx-aware AddFunc shims if robfig/cron grows a
// ctx parameter — the wrapper signature is the single place a ctx
// argument would land. Until then s.stopCtx remains a Scheduler
// struct field (see scheduler.go godoc on the field's anti-pattern
// rationale: robfig/cron callbacks have no ctx parameter slot).
//
// R20260527-COR-7 (#1299) panic-recovery boundary: this tick path does
// NOT wrap executeJobIDIfLive in a recover() — it relies on
// robfig/cron's Recover chain (NewScheduler installs
// robfigcron.Recover(cronLogger) in the WithChain() args). The
// TriggerNow path is the asymmetric case: it bypasses robfig's chain
// entirely, so executeIfNotDeletedOrPaused has its own
// recordTriggerNowPanic recover. A future refactor that splits the
// dispatch boundary differently (e.g. routes tick callbacks through a
// new entry that bypasses the chain) MUST add an explicit recover to
// preserve the "panicking job fails loud once and the surrounding
// goroutine still completes" contract that holds today.
func (s *Scheduler) newCronTickCallback(jobID string) func() {
	return func() {
		s.executeJobIDIfLive(jobID, false /* viaTriggerNow */, "cron")
	}
}

// findByPrefixLocked finds a job by ID prefix scoped to a specific chat.
//
// RETURNS (R249-CR-6, #950): exactly one of —
//   - (job, nil)                      the prefix uniquely identifies one job
//     in the (plat, chatID) scope.
//   - (nil, ErrJobNotFound)           no job in the scope matches the prefix,
//     OR a full-length ID exists but in a different chat scope (the foreign
//     job is masked as NotFound so callers can't probe its existence by ID).
//   - (nil, ErrAmbiguousPrefix)       a short prefix (typically 1-2 chars from
//     the IM-typed `naozhi cron pause <prefix>` flow) matches ≥2 jobs in the
//     scope; the wrapped message lists the colliding IDs so the operator can
//     disambiguate. Callers should errors.Is-check this and surface a
//     "please disambiguate" hint rather than treating it as NotFound.
//
// COMPLEXITY: the partial-prefix scan is linear in the number of jobs in the
// target chat (s.jobsByChat[chat]), bounded by maxJobsPerChat — NOT the full
// s.jobs table. The full-ID fast path below is O(1).
//
// LOCK: caller MUST hold s.mu (read or write). The body iterates the
// per-chat slice from s.jobsByChat directly without taking the mutex;
// every in-tree caller (DeleteJob / PauseJob / ResumeJob) already holds
// s.mu.Lock() across the find + mutate + persist window, so the *Locked
// suffix is a documentation contract, not a behaviour change. Renamed
// under R20260526-GO-002 to match the package convention (deleteJobLocked
// / pauseJobLocked / persistJobsLocked / …) so future callers see the
// locking requirement without grepping the call graph.
//
// R242-GO-9 (#558): scan is bounded by s.jobsByChat[chat] (typically
// 1-5 jobs/chat) rather than the full s.jobs map (up to maxJobsHardCap=
// 500). This drops the lock-time prefix scan to O(jobs-in-this-chat) so
// withJobByPrefix doesn't pin s.mu across the entire job table on every
// IM-prefix delete/pause/resume.
//
// R246-GO-16 (#705): full-ID fast path. When idPrefix is a complete
// hex job ID (length 2*hexIDEntropyBytes = 16) we hit s.jobs directly —
// O(1) map lookup instead of an O(N) range scan. Dashboard / HTTP
// callers already round-trip the full ID (the truncated prefix form
// only appears in the IM-typed CLI flow `naozhi cron pause abc` where
// the operator types a partial ID), so the common case is ID-shaped
// and benefits. The scan path is preserved verbatim for the partial-
// prefix case so the ambiguous-match error still fires identically.
// The Platform / ChatID match still has to gate the result — a full
// ID may hit the wrong chat scope (cross-chat probe) and must return
// ErrJobNotFound rather than the foreign job. Note we still hold the
// caller-supplied write lock during the lookup; the dashboard 1Hz
// read path is in s.mu.RLock (ListJobs / ListAllJobsWithNextRun) and
// the win is shorter blocking — the partial-prefix scan stays an
// honest O(N) tail.
func (s *Scheduler) findByPrefixLocked(idPrefix, plat, chatID string) (*Job, error) {
	if len(idPrefix) == 2*hexIDEntropyBytes {
		if j, ok := s.jobs[idPrefix]; ok {
			if j.Platform == plat && j.ChatID == chatID {
				return j, nil
			}
			// Full ID exists but in a different chat scope — surface
			// the same NotFound error the scan path would, so cross-
			// chat callers can't probe foreign-job existence by ID.
			return nil, fmt.Errorf("%w: prefix %q", ErrJobNotFound, idPrefix)
		}
		// Full-length ID with no map hit: still fall through to the
		// scan path. A pathological store load could in theory keep a
		// 16-char prefix that is NOT a full ID (e.g. data corruption
		// or a future ID-width bump where the operator types a 16-
		// char prefix of a 32-char ID), so the scan tail acts as the
		// safety net rather than short-circuiting on the map miss.
	}
	var matches []*Job
	for _, j := range s.jobsByChat[chatKeyFor(plat, chatID)] {
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
