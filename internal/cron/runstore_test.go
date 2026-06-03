package cron

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// runstore_test.go covers internal/cron/runstore.go (RFC: docs/rfc/cron-run-history.md).
// All tests use t.TempDir() + storePath = <tmp>/cron_jobs.json so the runStore
// roots itself at <tmp>/runs/. Tests use mustGenerateID()/mustGenerateRunID() to mint
// 16-hex IDs the runIDPattern accepts.

// newTestStore returns a runStore rooted under t.TempDir() with the given
// retention parameters and trim GC enabled by default. Callers wanting
// determinism flip enableTrimGC themselves after construction.
func newTestStore(t *testing.T, keepCount int, keepWindow time.Duration) *runStore {
	t.Helper()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := newRunStore(storePath, keepCount, keepWindow)
	if s == nil {
		t.Fatalf("newRunStore returned nil")
	}
	if s.disabled {
		t.Fatalf("newRunStore unexpectedly disabled")
	}
	return s
}

// makeRun builds a non-zero CronRun with a unique RunID under the given JobID.
// Caller can override fields after the helper returns.
func makeRun(jobID string, startedAt time.Time) *CronRun {
	return &CronRun{
		RunID:      mustGenerateRunID(),
		JobID:      jobID,
		State:      RunStateSucceeded,
		Trigger:    TriggerScheduled,
		StartedAt:  startedAt,
		EndedAt:    startedAt.Add(time.Second),
		DurationMS: 1000,
		Prompt:     "hello",
		WorkDir:    "/tmp/wd",
		Result:     "ok",
	}
}

// countJSONFiles walks dir and returns the number of *.json files.
func countJSONFiles(t *testing.T, dir string) int {
	t.Helper()
	count := 0
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(info.Name(), ".json") {
			count++
		}
		return nil
	})
	return count
}

// TestRunStore_AppendListRoundTrip ensures Append → List → Get preserves
// payload bytes (prompt/result/error_msg) end to end.
func TestRunStore_AppendListRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	run := makeRun(jobID, time.Now())
	run.State = RunStateFailed
	run.Trigger = TriggerManual
	run.Prompt = "review the diff"
	run.Result = "all good"
	run.ErrorClass = ErrClassSendError
	run.ErrorMsg = "boom"
	s.Append(run)

	summaries := s.List(jobID, 50, time.Time{})
	if len(summaries) != 1 {
		t.Fatalf("List len=%d want 1", len(summaries))
	}
	if summaries[0].RunID != run.RunID || summaries[0].State != RunStateFailed || summaries[0].Trigger != TriggerManual {
		t.Fatalf("summary mismatch: %+v", summaries[0])
	}

	got, err := s.Get(jobID, run.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Prompt != "review the diff" || got.Result != "all good" || got.ErrorMsg != "boom" {
		t.Fatalf("payload mismatch: prompt=%q result=%q errMsg=%q", got.Prompt, got.Result, got.ErrorMsg)
	}
	if got.ErrorClass != ErrClassSendError {
		t.Fatalf("ErrorClass = %v want %v", got.ErrorClass, ErrClassSendError)
	}
}

// TestRunStore_RejectInvalidIDs verifies non-hex jobID/runID payloads
// are rejected at Append time and never reach the disk.
func TestRunStore_RejectInvalidIDs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)

	// Non-hex jobID → rejected.
	run := makeRun("not-hex-id", time.Now())
	s.Append(run)
	if n := countJSONFiles(t, s.root); n != 0 {
		t.Fatalf("non-hex jobID produced %d files; want 0", n)
	}

	// Hex jobID but non-hex runID → rejected.
	jobID := mustGenerateID()
	run = makeRun(jobID, time.Now())
	run.RunID = "ZZZZZZZZZZZZZZZZ"
	s.Append(run)
	if n := countJSONFiles(t, s.root); n != 0 {
		t.Fatalf("non-hex runID produced %d files; want 0", n)
	}

	// List with non-hex jobID → empty result.
	if got := s.List("not-hex-id", 10, time.Time{}); got != nil {
		t.Fatalf("List(non-hex) = %v want nil", got)
	}
	// Get with non-hex jobID/runID → fs.ErrNotExist.
	if _, err := s.Get("not-hex-id", mustGenerateRunID()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Get(non-hex job) err = %v want fs.ErrNotExist", err)
	}
	if _, err := s.Get(mustGenerateID(), "ZZZZ"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Get(non-hex run) err = %v want fs.ErrNotExist", err)
	}
}

// TestRunStore_Disabled checks that an empty storePath disables every API
// without panicking.
func TestRunStore_Disabled(t *testing.T) {
	t.Parallel()
	s := newRunStore("", 0, 0)
	if s == nil || !s.disabled {
		t.Fatalf("expected disabled store; got %+v", s)
	}

	jobID := mustGenerateID()
	run := makeRun(jobID, time.Now())
	s.Append(run) // must no-op
	if got := s.List(jobID, 10, time.Time{}); got != nil {
		t.Fatalf("List on disabled = %v want nil", got)
	}
	if got := s.Recent(jobID, 5); got != nil {
		t.Fatalf("Recent on disabled = %v want nil", got)
	}
	if _, err := s.Get(jobID, run.RunID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Get on disabled err = %v want fs.ErrNotExist", err)
	}
	s.DeleteJob(jobID) // must no-op, no panic
}

// TestRunStore_OversizePayloadShrinksAndPersists writes a 50 KiB prompt
// and checks Append falls back to the truncate-and-retry path so a record
// is still persisted (with the truncated marker).
func TestRunStore_OversizePayloadShrinksAndPersists(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	huge := strings.Repeat("A", 50*1024)
	run := makeRun(jobID, time.Now())
	run.Prompt = huge
	s.Append(run)

	got, err := s.Get(jobID, run.RunID)
	if err != nil {
		t.Fatalf("Get after oversize append: %v", err)
	}
	if !strings.HasSuffix(got.Prompt, "…[truncated]") {
		start := len(got.Prompt) - 20
		if start < 0 {
			start = 0
		}
		t.Fatalf("prompt did not get truncated marker; len=%d suffix=%q", len(got.Prompt), got.Prompt[start:])
	}
	// truncateWithSuffix keeps the first 256 runes + sentinel; for ASCII
	// input that's 256 bytes + sentinel byte length.
	wantLen := 256 + len("…[truncated]")
	if len([]byte(got.Prompt)) != wantLen {
		t.Fatalf("truncated prompt bytes = %d want %d", len(got.Prompt), wantLen)
	}
}

// TestRunStore_OversizeMultiByteUTF8DoesNotSplitRune covers R221-FIX-P0-1:
// a multi-byte UTF-8 prompt that overflows maxRunBytes must not have its
// retry truncation slice mid-rune. With the old byte-based slice, "汉" (3
// bytes per rune) at byte 256 would land mid-sequence — json.Marshal would
// silently U+FFFD-replace, and in the worst case the second marshal would
// still exceed the cap, causing the run to be dropped entirely. Verify the
// retry path produces a valid UTF-8 string that's correctly persisted.
func TestRunStore_OversizeMultiByteUTF8DoesNotSplitRune(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	// "汉" is 3 bytes per rune; 50 KiB / 3 = ~17066 runes — well over the
	// 256-rune retry cap, and crucially every rune crosses byte boundaries.
	huge := strings.Repeat("汉", 18*1024)
	run := makeRun(jobID, time.Now())
	run.Prompt = huge
	s.Append(run)

	got, err := s.Get(jobID, run.RunID)
	if err != nil {
		t.Fatalf("Get after oversize multi-byte append: %v", err)
	}
	if !strings.HasSuffix(got.Prompt, "…[truncated]") {
		t.Fatalf("multi-byte prompt missing truncated marker; len=%d", len(got.Prompt))
	}
	if !utf8.ValidString(got.Prompt) {
		t.Fatalf("retry-truncated prompt is invalid UTF-8 — rune was split mid-sequence")
	}
	// Must keep exactly 256 runes (not 256 bytes) of the prefix, then sentinel.
	prefix := strings.TrimSuffix(got.Prompt, "…[truncated]")
	if got := utf8.RuneCountInString(prefix); got != 256 {
		t.Fatalf("retry truncation kept %d runes, want 256", got)
	}
}

// TestRunStore_RetentionByCount with keepCount=3, Append 5 runs, only the
// 3 newest survive after the synchronous trim. Disables enableTrimGC so we
// can stage all five files first, pin mtime, then trigger a single trim.
func TestRunStore_RetentionByCount(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 3, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	now := time.Now()
	runIDs := make([]string, 5)
	for i := 0; i < 5; i++ {
		run := makeRun(jobID, now.Add(time.Duration(i)*time.Second))
		runIDs[i] = run.RunID
		s.Append(run)
		// Pin mtime so the trim's mtime-desc rank order is deterministic.
		path := filepath.Join(s.root, jobID, run.RunID+".json")
		ts := now.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}

	// Single deterministic trim under a "now" far enough in the future that
	// the keepWindow=30d clause never trims by age.
	lock := s.jobLock(jobID)
	lock.Lock()
	s.trimJobLocked(jobID, now.Add(time.Hour))
	lock.Unlock()

	dir := filepath.Join(s.root, jobID)
	if n := countJSONFiles(t, dir); n != 3 {
		t.Fatalf("after retention got %d files; want 3", n)
	}
	// Newest 3 (indices 2,3,4) should survive.
	for i := 0; i < 2; i++ {
		_, err := s.Get(jobID, runIDs[i])
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("expected runIDs[%d] (%s) trimmed; err=%v", i, runIDs[i], err)
		}
	}
	for i := 2; i < 5; i++ {
		if _, err := s.Get(jobID, runIDs[i]); err != nil {
			t.Fatalf("expected runIDs[%d] (%s) kept; err=%v", i, runIDs[i], err)
		}
	}
}

// TestRunStore_RetentionByWindow with keepWindow=24h, Append 3 runs and
// rewind one mtime to 48h ago — that one must be trimmed.
func TestRunStore_RetentionByWindow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 24*time.Hour)
	s.enableTrimGC = false // run trim manually so mtime injection is honored
	jobID := mustGenerateID()

	now := time.Now()
	runs := make([]*CronRun, 3)
	for i := 0; i < 3; i++ {
		runs[i] = makeRun(jobID, now)
		s.Append(runs[i])
	}
	// Push runs[0] mtime back 48 hours.
	old := now.Add(-48 * time.Hour)
	oldPath := filepath.Join(s.root, jobID, runs[0].RunID+".json")
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	lock := s.jobLock(jobID)
	lock.Lock()
	s.trimJobLocked(jobID, now)
	lock.Unlock()

	if _, err := s.Get(jobID, runs[0].RunID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected aged run trimmed; err=%v", err)
	}
	for i := 1; i < 3; i++ {
		if _, err := s.Get(jobID, runs[i].RunID); err != nil {
			t.Fatalf("expected runs[%d] kept; err=%v", i, err)
		}
	}
}

// TestRunStore_RetentionAndConjunction verifies the AND policy: keepCount
// not exceeded, but window violated → trim the aged entries.
func TestRunStore_RetentionAndConjunction(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 10, 24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	now := time.Now()
	runs := make([]*CronRun, 5)
	for i := 0; i < 5; i++ {
		runs[i] = makeRun(jobID, now)
		s.Append(runs[i])
	}
	// Age runs[0] and runs[1] to 48h ago.
	old := now.Add(-48 * time.Hour)
	for _, idx := range []int{0, 1} {
		p := filepath.Join(s.root, jobID, runs[idx].RunID+".json")
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}

	lock := s.jobLock(jobID)
	lock.Lock()
	s.trimJobLocked(jobID, now)
	lock.Unlock()

	dir := filepath.Join(s.root, jobID)
	if n := countJSONFiles(t, dir); n != 3 {
		t.Fatalf("after AND trim got %d; want 3", n)
	}
	// Aged ones gone.
	for _, idx := range []int{0, 1} {
		if _, err := s.Get(jobID, runs[idx].RunID); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("expected aged runs[%d] trimmed; err=%v", idx, err)
		}
	}
	// Fresh ones survive.
	for _, idx := range []int{2, 3, 4} {
		if _, err := s.Get(jobID, runs[idx].RunID); err != nil {
			t.Fatalf("expected fresh runs[%d] kept; err=%v", idx, err)
		}
	}
}

// TestRunStore_DeleteJobRemovesSubtree wipes the whole runs/<jobID>/ tree.
func TestRunStore_DeleteJobRemovesSubtree(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	for i := 0; i < 3; i++ {
		s.Append(makeRun(jobID, time.Now()))
	}
	dir := filepath.Join(s.root, jobID)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("expected dir to exist; err=%v", err)
	}

	s.DeleteJob(jobID)
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dir still exists after DeleteJob: err=%v", err)
	}
}

// TestRunStore_DeleteJobReclaimsJobLock pins R249-ARCH-3 (#971): DeleteJob
// must drop the per-job *sync.Mutex from jobLocks so a long-lived deployment
// that creates and deletes many jobs does not grow the map without bound.
// Before the fix jobLocks entries were "never reclaimed", contradicting the
// claimed maxJobsHardCap bound.
func TestRunStore_DeleteJobReclaimsJobLock(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	s.Append(makeRun(jobID, time.Now()))
	// Appending takes jobLock, so the entry must exist now.
	if _, ok := s.jobLocks.Load(jobID); !ok {
		t.Fatalf("expected jobLocks entry after Append")
	}

	s.DeleteJob(jobID)
	if _, ok := s.jobLocks.Load(jobID); ok {
		t.Fatalf("jobLocks entry still present after DeleteJob; per-job mutex leaked")
	}

	// Also confirm no entries linger in aggregate.
	count := 0
	s.jobLocks.Range(func(_, _ any) bool { count++; return true })
	if count != 0 {
		t.Fatalf("jobLocks has %d residual entries after deleting the only job; want 0", count)
	}
}

// TestRunStore_DeleteJobIdempotent — DeleteJob on a non-existent ID must
// not panic or return an error.
func TestRunStore_DeleteJobIdempotent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	// The lock entry is created then reclaimed; the rmdir is a no-op.
	s.DeleteJob(mustGenerateID())
	// And again — still fine.
	s.DeleteJob(mustGenerateID())
}

// TestRunStore_GetMissingReturnsErrNotExist — fs.ErrNotExist must propagate
// unchanged when the file simply isn't there.
func TestRunStore_GetMissingReturnsErrNotExist(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	_, err := s.Get(mustGenerateID(), mustGenerateRunID())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Get(missing) err = %v want fs.ErrNotExist", err)
	}
}

// TestRunStore_GetCorruptReturnsErrCorruptRun — a malformed JSON file
// surfaces as ErrCorruptRun.
func TestRunStore_GetCorruptReturnsErrCorruptRun(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()
	runID := mustGenerateRunID()

	dir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, runID+".json")
	if err := os.WriteFile(path, []byte("{this is not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := s.Get(jobID, runID)
	if !errors.Is(err, ErrCorruptRun) {
		t.Fatalf("Get(corrupt) err = %v want ErrCorruptRun", err)
	}
}

// TestRunStore_GetOversizeReturnsErrCorruptRun — a valid-JSON but
// >MaxRunRecordBytes file also surfaces as ErrCorruptRun.
func TestRunStore_GetOversizeReturnsErrCorruptRun(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()
	runID := mustGenerateRunID()

	huge := &CronRun{
		RunID:     runID,
		JobID:     jobID,
		State:     RunStateSucceeded,
		StartedAt: time.Now(),
		// Pump it well above the 32 KiB cap so we don't accidentally fall under.
		Result: strings.Repeat("X", MaxRunRecordBytes+1024),
	}
	data, err := json.Marshal(huge)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if int64(len(data)) <= MaxRunRecordBytes {
		t.Fatalf("test fixture too small: %d <= %d", len(data), MaxRunRecordBytes)
	}
	dir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, runID+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := s.Get(jobID, runID); !errors.Is(err, ErrCorruptRun) {
		t.Fatalf("Get(oversize) err = %v want ErrCorruptRun", err)
	}
}

// TestRunStore_ListSkipsCorruptEntries — one good record + one corrupt
// record in the same dir; List must yield only the good summary.
func TestRunStore_ListSkipsCorruptEntries(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	good := makeRun(jobID, time.Now())
	s.Append(good)

	// Plant a sibling corrupt file.
	dir := filepath.Join(s.root, jobID)
	bad := filepath.Join(dir, mustGenerateRunID()+".json")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := s.List(jobID, 50, time.Time{})
	if len(got) != 1 {
		t.Fatalf("List len=%d want 1 (corrupt skipped)", len(got))
	}
	if got[0].RunID != good.RunID {
		t.Fatalf("List returned wrong run: %s", got[0].RunID)
	}
}

// TestRunStore_DecodeParallel_UnreadableNotCorrupt pins R20260603150052-CR-7
// (#1693): a slot whose file is unreadable (EACCES) must be counted in
// unreadableCount, NOT in corruptCount. Before the fix both error classes were
// merged into corruptCount, causing operators to see "skipped corrupt files"
// in logs for transient IO problems. We use chmod 0000 to simulate EACCES.
//
// diskDecodeParallelThreshold == 16, so we need > 16 candidates to reach
// decodeRunsParallel. We append 18 runs and make one unreadable.
func TestRunStore_DecodeParallel_UnreadableNotCorrupt(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chmod 0000 does not deny access")
	}
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	// Append 18 runs (> diskDecodeParallelThreshold=16) so that
	// diskListNewestFirst fans out via decodeRunsParallel.
	const total = 18
	now := time.Now()
	runIDs := make([]string, total)
	for i := 0; i < total; i++ {
		r := makeRun(jobID, now.Add(time.Duration(i)*time.Second))
		s.Append(r)
		runIDs[i] = r.RunID
	}

	// Make the last (newest) run unreadable — permission error, not corruption.
	unreadablePath := filepath.Join(s.root, jobID, runIDs[total-1]+".json")
	if err := os.Chmod(unreadablePath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadablePath, 0o600) })

	// Call diskListNewestFirst directly — bypasses the in-memory cache and
	// exercises the decodeRunsParallel worker path.
	// The unreadable file must land in unreadableCount, not corruptCount.
	rows, corruptCount, unreadableCount := s.diskListNewestFirst(jobID, 100, time.Time{})
	if corruptCount != 0 {
		t.Fatalf("corruptCount=%d want 0 (EACCES is not JSON corruption)", corruptCount)
	}
	if unreadableCount != 1 {
		t.Fatalf("unreadableCount=%d want 1 (unreadable file must be counted separately)", unreadableCount)
	}
	if len(rows) != total-1 {
		t.Fatalf("rows len=%d want %d (all readable files returned)", len(rows), total-1)
	}
}

// TestRunStore_ListBeforeCutoff — stagger StartedAt by 1h, ask for entries
// strictly before "now-2h", expect only those satisfying StartedAt < cutoff.
//
// R238-GO-8 (#796): mtimes are pinned to match StartedAt to mirror the
// production invariant (Append fires at finishRun time so mtime ≈
// EndedAt ≥ StartedAt, bounded by ExecTimeout). Without the pin the
// raw Append path stamps mtime = wall-clock-now, which mismatches the
// synthetic backdated StartedAt and would intersect the new mtime
// coarse-filter — a test-fixture artefact, not a production case.
func TestRunStore_ListBeforeCutoff(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	for i := 0; i < 5; i++ {
		// runs[0] = now-4h, runs[1] = now-3h, ... runs[4] = now.
		startedAt := now.Add(-time.Duration(4-i) * time.Hour)
		run := makeRun(jobID, startedAt)
		s.Append(run)
		// Pin mtime ≈ StartedAt to match production semantics. Required
		// after R238-GO-8 (#796) added a coarse mtime gate before
		// readRun.
		path := filepath.Join(s.root, jobID, run.RunID+".json")
		if err := os.Chtimes(path, startedAt, startedAt); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}

	cutoff := now.Add(-2 * time.Hour)
	got := s.List(jobID, 10, cutoff)
	for _, sm := range got {
		if !sm.StartedAt.Before(cutoff) {
			t.Fatalf("List returned entry with StartedAt %v not < cutoff %v", sm.StartedAt, cutoff)
		}
	}
	// runs at now-4h, now-3h are strictly < cutoff (now-2h). runs at
	// now-2h (boundary; Before is strict so excluded), now-1h, now are not.
	if len(got) != 2 {
		t.Fatalf("List(before cutoff) len=%d want 2; got %+v", len(got), got)
	}
}

// TestRunStore_ConcurrentAppendsSameJobAreSerialised — 50 goroutines each
// Append once; final disk count must equal 50 (modulo trimming, so use a
// generous keepCount). Run with -race to surface mutex bugs.
func TestRunStore_ConcurrentAppendsSameJobAreSerialised(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 1000, 30*24*time.Hour)
	jobID := mustGenerateID()

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			s.Append(makeRun(jobID, time.Now()))
		}()
	}
	wg.Wait()

	dir := filepath.Join(s.root, jobID)
	if n := countJSONFiles(t, dir); n != N {
		t.Fatalf("concurrent appends produced %d files; want %d", n, N)
	}
}

// TestRunStore_TrimAllScansAllJobs — three jobs, each with five runs; one
// of them has two stale entries. trimAll must touch only that job.
func TestRunStore_TrimAllScansAllJobs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 10, 24*time.Hour)
	s.enableTrimGC = false

	jobIDs := []string{mustGenerateID(), mustGenerateID(), mustGenerateID()}
	allRuns := make(map[string][]*CronRun, 3)

	now := time.Now()
	for _, jid := range jobIDs {
		runs := make([]*CronRun, 5)
		for i := 0; i < 5; i++ {
			runs[i] = makeRun(jid, now)
			s.Append(runs[i])
		}
		allRuns[jid] = runs
	}

	// Age two entries under jobIDs[1] only.
	old := now.Add(-48 * time.Hour)
	target := jobIDs[1]
	for _, idx := range []int{0, 1} {
		p := filepath.Join(s.root, target, allRuns[target][idx].RunID+".json")
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}

	s.trimAll(now)

	// jobIDs[0] and jobIDs[2] untouched (5 each).
	for _, jid := range []string{jobIDs[0], jobIDs[2]} {
		dir := filepath.Join(s.root, jid)
		if n := countJSONFiles(t, dir); n != 5 {
			t.Fatalf("trimAll modified untouched job %s: %d files", jid, n)
		}
	}
	// jobIDs[1] shrunk to 3.
	dir := filepath.Join(s.root, target)
	if n := countJSONFiles(t, dir); n != 3 {
		t.Fatalf("trimAll on aged job: %d files want 3", n)
	}
}

// TestRunStore_MarshalRunPooledMatchesStdlib — #1043 / R240-PERF-6:
// the pooled marshal helper must produce byte-identical output to
// json.Marshal so on-disk records are unchanged. Drives the helper
// across two consecutive runs to exercise the pool-recycle path.
func TestRunStore_MarshalRunPooledMatchesStdlib(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []*CronRun{
		makeRun("0123456789abcdef", now),
		{
			JobID:     "fedcba9876543210",
			RunID:     "abc123",
			StartedAt: now,
			Result:    "html-escape <script>tag</script>&friend",
			Prompt:    "p",
		},
	}
	for _, r := range cases {
		want, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("stdlib json.Marshal: %v", err)
		}
		got, err := marshalRunPooled(r)
		if err != nil {
			t.Fatalf("marshalRunPooled: %v", err)
		}
		if string(want) != string(got) {
			t.Fatalf("byte mismatch\nwant: %s\n got: %s", want, got)
		}
		// Second pass exercises the pool-recycled buffer.
		got2, err := marshalRunPooled(r)
		if err != nil {
			t.Fatalf("marshalRunPooled (recycle): %v", err)
		}
		if string(want) != string(got2) {
			t.Fatalf("byte mismatch on recycled buf\nwant: %s\n got: %s", want, got2)
		}
		// Returned slice must be independent of the pooled buffer:
		// mutating got1 must not corrupt got2.
		if len(got) > 0 {
			got[0] = 'X'
		}
		if string(want) != string(got2) {
			t.Fatalf("returned slice not independent of pool buffer")
		}
	}
}

// TestRunStore_TrimAllCtxCancelled — #1019 / R234-GO-3: trimAllCtx exits
// early at the next job-entry boundary when the supplied ctx is cancelled.
// We seed many job dirs each with a stale entry; cancel ctx before calling;
// verify NOT all jobs were trimmed (early-exit path) — i.e. at least one
// job retains its full pre-trim file count.
func TestRunStore_TrimAllCtxCancelled(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 10, 24*time.Hour)
	s.enableTrimGC = false

	now := time.Now()
	old := now.Add(-48 * time.Hour)
	const N = 12
	jobIDs := make([]string, N)
	for i := 0; i < N; i++ {
		jid := mustGenerateID()
		jobIDs[i] = jid
		// 2 stale + 1 fresh entry per job
		for j := 0; j < 3; j++ {
			r := makeRun(jid, now)
			s.Append(r)
			if j < 2 {
				p := filepath.Join(s.root, jid, r.RunID+".json")
				if err := os.Chtimes(p, old, old); err != nil {
					t.Fatalf("Chtimes: %v", err)
				}
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call → first ctx.Err() check fires immediately
	s.trimAllCtx(ctx, now)

	// With pre-cancelled ctx, the first iteration's ctx.Err() check returns
	// before any trimJobUnderLock runs, so every job retains its 3 files.
	untouched := 0
	for _, jid := range jobIDs {
		dir := filepath.Join(s.root, jid)
		if countJSONFiles(t, dir) == 3 {
			untouched++
		}
	}
	if untouched != N {
		t.Fatalf("trimAllCtx with cancelled ctx should be a no-op; "+
			"untouched=%d want %d", untouched, N)
	}

	// Sanity: same store with fresh ctx does perform the trim.
	s.trimAllCtx(context.Background(), now)
	for _, jid := range jobIDs {
		dir := filepath.Join(s.root, jid)
		if n := countJSONFiles(t, dir); n != 1 {
			t.Fatalf("trimAllCtx with bg ctx: job %s has %d files, want 1", jid, n)
		}
	}
}

// TestRunStore_RecentReturnsNewestFirst — Recent honours mtime-desc order
// produced by List under the hood.
func TestRunStore_RecentReturnsNewestFirst(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	now := time.Now()
	runs := make([]*CronRun, 5)
	for i := 0; i < 5; i++ {
		runs[i] = makeRun(jobID, now.Add(time.Duration(i)*time.Minute))
		s.Append(runs[i])
		// Pin mtime so List's mtime-desc sort is deterministic.
		p := filepath.Join(s.root, jobID, runs[i].RunID+".json")
		ts := now.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}

	got := s.Recent(jobID, 3)
	if len(got) != 3 {
		t.Fatalf("Recent len=%d want 3", len(got))
	}
	// Expect runs[4], runs[3], runs[2].
	want := []string{runs[4].RunID, runs[3].RunID, runs[2].RunID}
	for i, sm := range got {
		if sm.RunID != want[i] {
			t.Fatalf("Recent[%d].RunID = %s want %s", i, sm.RunID, want[i])
		}
	}
	// And StartedAt is monotonically decreasing.
	for i := 1; i < len(got); i++ {
		if !got[i-1].StartedAt.After(got[i].StartedAt) {
			t.Fatalf("Recent not newest-first at i=%d: %v vs %v", i, got[i-1].StartedAt, got[i].StartedAt)
		}
	}
}

// TestRunStore_ListHonoursConfiguredKeepCount is the regression test for
// R249-ARCH-1 (#969): once SchedulerConfig.RunsKeepCount is plumbed into
// s.keepCount, List / RecentSessionIDs must clamp the requested limit to the
// configured retention cap rather than the hardcoded DefaultRunsKeepCount
// (200). An operator who raised retention above 200 previously could never
// page the extra rows because the read side silently truncated at 200.
func TestRunStore_ListHonoursConfiguredKeepCount(t *testing.T) {
	t.Parallel()
	const keep = DefaultRunsKeepCount + 50 // 250 — above the old hardcoded clamp
	s := newTestStore(t, keep, 30*24*time.Hour)
	s.enableTrimGC = false // keep every append so the read clamp is the only limiter
	jobID := mustGenerateID()

	const total = DefaultRunsKeepCount + 10 // 210 — more than the old clamp, fewer than keep
	now := time.Now()
	for i := 0; i < total; i++ {
		run := makeRun(jobID, now.Add(time.Duration(i)*time.Second))
		run.SessionID = run.RunID // distinct non-empty session per run
		s.Append(run)
	}

	// Request more than DefaultRunsKeepCount; the clamp must now allow up to
	// s.keepCount, so all `total` rows come back.
	got := s.List(jobID, keep, time.Time{})
	if len(got) != total {
		t.Fatalf("List(limit=%d) returned %d rows; want %d (clamp must honour keepCount, not DefaultRunsKeepCount)", keep, len(got), total)
	}

	// RecentSessionIDs mirrors the same clamp.
	sids := s.RecentSessionIDs(jobID, keep)
	if len(sids) != total {
		t.Fatalf("RecentSessionIDs(n=%d) returned %d ids; want %d", keep, len(sids), total)
	}
}

// newEntryFromRows builds a warm recentCacheEntry whose ring is seeded
// from rows (newest-first). appendsSinceTrim sets the bookkeeping
// counter for skipAppendTrim test cases. Pure test helper for R242-GO-8
// — production code seeds via ringSeed inside warmCache.
func newEntryFromRows(rows []CronRunSummary, appendsSinceTrim int) *recentCacheEntry {
	e := &recentCacheEntry{warm: true, appendsSinceTrim: appendsSinceTrim}
	// Cap the ring to len(rows) at minimum so iteration works; production
	// uses keepCount, but skipAppendTrim only reads count and ringRead
	// which both honour cap(ring).
	cap := len(rows)
	if cap == 0 {
		cap = 1
	}
	e.ringSeed(rows, cap)
	return e
}

// TestRunStore_SkipAppendTrim_Conditions covers the four return branches of
// runStore.skipAppendTrim, the optimisation introduced by R232-PERF-8 that
// lets Append skip the per-call ReadDir when the cache shows we're well
// under keepCount and keepWindow. R233B-CR-7 flagged these branches as
// untested; this table-driven test pins them explicitly so a future tweak
// to the heuristic cannot silently regress.
//
// Each case constructs a minimal *runStore + recentCacheEntry directly (no
// disk I/O) and asserts the boolean result, then verifies the
// appendsSinceTrim bookkeeping side effect that drives the periodic
// "force one trim every appendTrimBatch" forcing condition.
func TestRunStore_SkipAppendTrim_Conditions(t *testing.T) {
	now := time.Now()

	// "happy" entry: cache warm, comfortably under keepCount, oldest row is
	// inside keepWindow (newer than cutoff). All three skip conditions pass.
	//
	// R242-GO-8: cache storage is now a ring buffer (entry.ring + head +
	// count). The newEntryFromRows helper centralises the slice → ring
	// translation so test cases keep their declarative shape.
	makeHappyEntry := func() *recentCacheEntry {
		return newEntryFromRows([]CronRunSummary{
			{RunID: "a", EndedAt: now.Add(-1 * time.Minute)},
			{RunID: "b", EndedAt: now.Add(-2 * time.Minute)},
		}, 0)
	}

	cases := []struct {
		name        string
		entry       *recentCacheEntry
		notWarm     bool
		keepCount   int
		keepWindow  time.Duration
		wantSkip    bool
		wantCounter int // appendsSinceTrim after the call
	}{
		{
			name:        "cold cache forces full trim",
			entry:       &recentCacheEntry{warm: false},
			keepCount:   100,
			keepWindow:  24 * time.Hour,
			wantSkip:    false,
			wantCounter: 0, // not warm: counter untouched (R242-GO-8: cold ring stays nil)
		},
		{
			// R20260527-PERF-24 (#1295): when both cap and window proofs
			// hold the counter resets — there is no benefit to advancing
			// toward an inevitable forced scan that will find no work.
			name:        "warm + headroom + within window: skip",
			entry:       makeHappyEntry(),
			keepCount:   100,
			keepWindow:  24 * time.Hour,
			wantSkip:    true,
			wantCounter: 0, // both proofs hold → no need to track drift
		},
		{
			name: "near keepCount cap: do not skip",
			entry: newEntryFromRows(func() []CronRunSummary {
				// keepCount=15 + appendTrimBatch(=10) → 15-10+1 = 6 rows triggers gate
				r := make([]CronRunSummary, 6)
				for i := range r {
					r[i] = CronRunSummary{EndedAt: now.Add(-time.Duration(i) * time.Minute)}
				}
				return r
			}(), 0),
			keepCount:   15,
			keepWindow:  24 * time.Hour,
			wantSkip:    false,
			wantCounter: 0, // forced trim resets counter
		},
		{
			name: "oldest row beyond keepWindow: do not skip",
			entry: newEntryFromRows([]CronRunSummary{
				{RunID: "a", EndedAt: now.Add(-30 * time.Second)},
				{RunID: "b", EndedAt: now.Add(-2 * time.Hour)}, // older than keepWindow
			}, 0),
			keepCount:   100,
			keepWindow:  1 * time.Hour, // cutoff = now-1h, oldest at now-2h is older
			wantSkip:    false,
			wantCounter: 0,
		},
		{
			// R20260527-PERF-24 (#1295): when cap+window are both safe the
			// appendTrimBatch boundary no longer forces a disk scan — the
			// scan would find nothing to evict, so paying ReadDir+Stat
			// every 10 calls was pure overhead. Counter resets so we
			// don't accumulate phantom drift toward a forced no-op scan.
			name:        "appendTrimBatch reached but cache clean: still skip",
			entry:       newEntryFromRows([]CronRunSummary{{EndedAt: now}}, appendTrimBatch-1),
			keepCount:   100,
			keepWindow:  24 * time.Hour,
			wantSkip:    true,
			wantCounter: 0, // both proofs hold → reset, no force trim
		},
		{
			// Boundary still forces a trim when window proof fails — the
			// reason for the periodic forcing was age-based eviction for
			// jobs that never approach keepCount; that behaviour is
			// preserved when the cache cannot prove safety.
			name: "appendTrimBatch reached + oldest beyond window: force trim",
			entry: newEntryFromRows([]CronRunSummary{
				{EndedAt: now.Add(-30 * time.Second)},
				{EndedAt: now.Add(-2 * time.Hour)},
			}, appendTrimBatch-1),
			keepCount:   100,
			keepWindow:  1 * time.Hour,
			wantSkip:    false,
			wantCounter: 0,
		},
		{
			// R090031-CR-3: capSafe used < instead of <=; when
			// count+appendTrimBatch == keepCount the cache can still
			// prove no removal is needed (trimJobLocked only removes
			// when len(items) > keepCount), so we must skip.
			name: "count+appendTrimBatch == keepCount: capSafe boundary skip",
			entry: newEntryFromRows(func() []CronRunSummary {
				// keepCount=10, appendTrimBatch=10 → count=0: 0+10==10 → capSafe
				r := make([]CronRunSummary, 0)
				return r
			}(), 0),
			keepCount:   appendTrimBatch,
			keepWindow:  24 * time.Hour,
			wantSkip:    true,
			wantCounter: 0,
		},
		{
			// count+appendTrimBatch exactly == keepCount with rows present.
			// With <= semantics capSafe=true; windowSafe=true → skip.
			name: "count+appendTrimBatch == keepCount with fresh rows: skip",
			entry: newEntryFromRows(func() []CronRunSummary {
				// keepCount=15, count=5: 5+10==15 → capSafe with <=
				r := make([]CronRunSummary, 5)
				for i := range r {
					r[i] = CronRunSummary{EndedAt: now.Add(-time.Duration(i+1) * time.Minute)}
				}
				return r
			}(), 0),
			keepCount:   15,
			keepWindow:  24 * time.Hour,
			wantSkip:    true,
			wantCounter: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &runStore{
				keepCount:  tc.keepCount,
				keepWindow: tc.keepWindow,
			}
			s.recentCache.Store("job", tc.entry)
			// R239-GO-5: skipAppendTrim contract requires caller hold
			// jobLock; the production callsite (Append) does. Match
			// here so the assertJobLockHeld guard inside doesn't fire.
			lock := s.jobLock("job")
			lock.Lock()
			got := s.skipAppendTrim("job", now)
			lock.Unlock()
			if got != tc.wantSkip {
				t.Errorf("skipAppendTrim = %v, want %v", got, tc.wantSkip)
			}
			if tc.entry.appendsSinceTrim != tc.wantCounter {
				t.Errorf("appendsSinceTrim = %d, want %d",
					tc.entry.appendsSinceTrim, tc.wantCounter)
			}
		})
	}
}

// TestRunStore_SkipAppendTrim_MissingEntry asserts the missing-jobID branch:
// when the cache has no entry for the given job, skipAppendTrim must return
// false (forcing a full trim) and not panic on the nil load.
func TestRunStore_SkipAppendTrim_MissingEntry(t *testing.T) {
	s := &runStore{keepCount: 100, keepWindow: 24 * time.Hour}
	// R239-GO-5: skipAppendTrim contract requires caller hold jobLock.
	lock := s.jobLock("never-seen")
	lock.Lock()
	defer lock.Unlock()
	if s.skipAppendTrim("never-seen", time.Now()) {
		t.Error("expected false for unknown jobID")
	}
}

// TestRunStore_ReadRunNoLstat_MatchesReadRun pins the contract that the
// no-Lstat variant returns byte-identical run records for a normal regular
// .json file: callers in diskListNewestFirst must observe no parsing drift
// across the optimisation. R245-PERF-9.
func TestRunStore_ReadRunNoLstat_MatchesReadRun(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	run := makeRun(jobID, time.Now())
	s.Append(run)

	path := filepath.Join(s.root, jobID, run.RunID+".json")
	withLstat, err := s.readRun(path)
	if err != nil {
		t.Fatalf("readRun: %v", err)
	}
	noLstat, err := s.readRunNoLstat(path)
	if err != nil {
		t.Fatalf("readRunNoLstat: %v", err)
	}
	a, _ := json.Marshal(withLstat)
	b, _ := json.Marshal(noLstat)
	if string(a) != string(b) {
		t.Fatalf("readRunNoLstat diverges from readRun:\nwithLstat=%s\n noLstat=%s", a, b)
	}
}

// TestRunStore_ReadRunNoLstat_OverCap asserts the size-cap path still rejects
// oversized payloads as ErrCorruptRun without the Lstat. The size enforcement
// lives in parseRunBytes shared between both readers.
func TestRunStore_ReadRunNoLstat_OverCap(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.maxRunBytes = 64 // tight cap; valid CronRun JSON is well over 64 bytes.
	jobID := mustGenerateID()
	if err := os.MkdirAll(filepath.Join(s.root, jobID), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runID := mustGenerateRunID()
	path := filepath.Join(s.root, jobID, runID+".json")
	payload := []byte(`{"run_id":"` + runID + `","job_id":"` + jobID + `","state":"succeeded","prompt":"this prompt is intentionally long enough to bust the tight cap"}`)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := s.readRunNoLstat(path)
	if !errors.Is(err, ErrCorruptRun) {
		t.Fatalf("readRunNoLstat over-cap err = %v, want ErrCorruptRun wrap", err)
	}
}

// TestR245Sec1_NewRunStoreRejectsSymlinkRunsDir — regression for #825.
// If a malicious operator (or post-compromise attacker) pre-creates
// `<dataDir>/runs` as a symlink to /etc, every Append would write a
// CronRun JSON outside the data dir. newRunStore must Lstat runs/ and
// disable the store when it's not a plain directory.
func TestR245Sec1_NewRunStoreRejectsSymlinkRunsDir(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	// Pre-create runs/ as a symlink to a sibling tempdir. MkdirAll on a
	// symlink-to-existing-dir succeeds silently, so without the Lstat
	// guard newRunStore would happily proceed.
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dataDir, "runs")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	storePath := filepath.Join(dataDir, "cron.json")
	s := newRunStore(storePath, 0, 0)
	if !s.disabled {
		t.Fatalf("newRunStore must disable when runs/ is a symlink; got enabled root=%q", s.root)
	}
	// Ensure subsequent Append is a safe no-op rather than writing into
	// the symlink target.
	s.Append(&CronRun{
		RunID: mustGenerateRunID(), JobID: mustGenerateID(),
		State: RunStateSucceeded, StartedAt: time.Now(),
	})
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("readdir target: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Append on disabled store wrote %d entries to symlink target", len(entries))
	}
}

// TestR245Sec1_NewRunStoreNormalisesDotDot — regression for #825. A
// storePath with `..` segments must be cleaned by filepath.Abs so the
// derived runs/ root cannot escape the intended data dir.
func TestR245Sec1_NewRunStoreNormalisesDotDot(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	// Construct a path with traversal: <tmp>/x/../cron.json should land
	// the runs root at <tmp>/runs, NOT at <tmp>/x/../runs (which would
	// equal <tmp>/runs only after cleaning — point of the test is that
	// the stored root is canonicalised).
	dirty := filepath.Join(tmp, "x", "..", "cron.json")
	s := newRunStore(dirty, 0, 0)
	if s.disabled {
		t.Fatalf("newRunStore must succeed with cleanable path; got disabled")
	}
	wantRoot := filepath.Join(tmp, "runs")
	if s.root != wantRoot {
		t.Errorf("root = %q, want %q (filepath.Abs must normalise `..`)", s.root, wantRoot)
	}
}

// TestR238Sec7_ReadRunRefusesSymlink — regression for #827. readRun
// (Get's entry path) must reject a symlink final component without
// dereferencing it. Pre-fix used Lstat + ReadFile which left a TOCTOU
// window — Lstat could see a regular file, then an attacker swaps the
// path to a symlink before ReadFile and the contents of /etc/passwd
// (or any sensitive file) leaks into the run record. Post-fix uses
// OpenFile(O_NOFOLLOW) + Fstat so the bytes we parse come from exactly
// the inode whose mode we validated.
func TestR238Sec7_ReadRunRefusesSymlink(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()
	runID := mustGenerateRunID()

	dir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Plant a "secret" file outside runs/ that the attacker would want
	// readRun to dereference.
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.json")
	if err := os.WriteFile(secret, []byte(`{"run_id":"deadbeef","job_id":"deadbeef","state":"succeeded"}`), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	// The attacker-planted symlink lives where Get would look for the
	// run record. O_NOFOLLOW must refuse to traverse it.
	link := filepath.Join(dir, runID+".json")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := s.Get(jobID, runID); !errors.Is(err, ErrCorruptRun) {
		t.Fatalf("Get on symlink err = %v want ErrCorruptRun (O_NOFOLLOW must refuse)", err)
	}
}

// TestRunStore_DiskList_BeforeStartedAtFilter (R246-CR-008 / #745) pins
// the corrected pagination semantics. Pre-fix the coarse mtime gate
// short-circuited entries whose mtime was at or after `before`, but the
// strict cutoff filtered on StartedAt — a long-running job with
// StartedAt < before but mtime ≥ before was silently dropped from the
// page. After dropping the coarse mtime gate, every candidate is read
// and StartedAt drives the cut. The corrupt-newest entries continue to
// be skipped, just now via the strict ErrCorruptRun branch (so
// corruptCount bumps), preserving the operator-observable contract that
// corrupt records do not poison list output.
func TestRunStore_DiskList_BeforeStartedAtFilter(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	now := time.Now()
	startedAts := []time.Time{
		now,
		now.Add(-30 * time.Minute),
		now.Add(-2 * time.Hour),
		now.Add(-3 * time.Hour),
	}
	runIDs := make([]string, 4)
	for i, sa := range startedAts {
		run := makeRun(jobID, sa)
		runIDs[i] = run.RunID
		s.Append(run)
		// Pin mtime ≈ StartedAt to mirror the production invariant.
		path := filepath.Join(s.root, jobID, run.RunID+".json")
		if err := os.Chtimes(path, sa, sa); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}
	// Corrupt the JSON of the two newest. After the R246-CR-008 fix the
	// coarse gate is gone — diskListNewestFirst MUST still exclude them
	// from `rows`, just via the readRun ErrCorruptRun branch which bumps
	// corruptCount. Test asserts the list payload remains unchanged
	// (operator-visible contract preserved).
	for i := 0; i < 2; i++ {
		path := filepath.Join(s.root, jobID, runIDs[i]+".json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatalf("corrupt write: %v", err)
		}
		if err := os.Chtimes(path, startedAts[i], startedAts[i]); err != nil {
			t.Fatalf("re-Chtimes: %v", err)
		}
	}

	before := now.Add(-time.Hour)
	rows, corruptCount, _ := s.diskListNewestFirst(jobID, 100, before)
	// The two corrupt newer entries are read+rejected (corruptCount bumps).
	// Pre-fix this was 0 (coarse gate skipped them); post-fix it's 2 because
	// the strict StartedAt filter is the sole truth — we MUST read each
	// candidate to know its StartedAt.
	if corruptCount != 2 {
		t.Fatalf("corruptCount=%d, want 2 — strict StartedAt filter must read each candidate; coarse mtime gate removed for correctness", corruptCount)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("rows=%d want %d (only StartedAt<before runs should land)", got, want)
	}
	gotIDs := map[string]bool{rows[0].RunID: true, rows[1].RunID: true}
	if !gotIDs[runIDs[2]] || !gotIDs[runIDs[3]] {
		t.Fatalf("rows missing expected older runs: got=%v want %s,%s", rows, runIDs[2], runIDs[3])
	}
}

// TestRunStore_DiskList_BeforeStartedAtMtimeDivergence (R246-CR-008 /
// #745) is the regression that the old coarse-mtime gate hid: a run
// with StartedAt < before but mtime ≥ before MUST still appear in the
// page. Pre-fix the entry was silently dropped because mtime ≥ before
// short-circuited before the StartedAt strict filter ran. Mimic the
// long-running-job scenario by writing a real run, then bumping mtime
// past `before` while leaving StartedAt earlier.
func TestRunStore_DiskList_BeforeStartedAtMtimeDivergence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	now := time.Now()
	// StartedAt 2h ago — clearly inside the page (before = 1h ago).
	startedAt := now.Add(-2 * time.Hour)
	run := makeRun(jobID, startedAt)
	s.Append(run)
	// Bump mtime to NOW (post-`before`) to simulate a late finishRun
	// rename / process-restart re-touch. StartedAt in the JSON is
	// untouched — the bug surface.
	path := filepath.Join(s.root, jobID, run.RunID+".json")
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	before := now.Add(-time.Hour)
	rows, corruptCount, _ := s.diskListNewestFirst(jobID, 100, before)
	if corruptCount != 0 {
		t.Fatalf("unexpected corruptCount=%d", corruptCount)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d want 1 — StartedAt<before run must NOT be skipped just because mtime>=before", len(rows))
	}
	if rows[0].RunID != run.RunID {
		t.Fatalf("rows[0].RunID=%q want %q", rows[0].RunID, run.RunID)
	}
}
