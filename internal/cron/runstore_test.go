package cron

import (
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
// roots itself at <tmp>/runs/. Tests use generateID()/generateRunID() to mint
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
		RunID:      generateRunID(),
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
	jobID := generateID()

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
	jobID := generateID()
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
	if _, err := s.Get("not-hex-id", generateRunID()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Get(non-hex job) err = %v want fs.ErrNotExist", err)
	}
	if _, err := s.Get(generateID(), "ZZZZ"); !errors.Is(err, fs.ErrNotExist) {
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

	jobID := generateID()
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
	jobID := generateID()

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
	// truncateForRetry keeps the first 256 runes + sentinel; for ASCII
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
	jobID := generateID()

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
	jobID := generateID()

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
	jobID := generateID()

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
	jobID := generateID()

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
	jobID := generateID()

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

// TestRunStore_DeleteJobIdempotent — DeleteJob on a non-existent ID must
// not panic or return an error.
func TestRunStore_DeleteJobIdempotent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	// The lock entry will be created; the rmdir is a no-op.
	s.DeleteJob(generateID())
	// And again — still fine.
	s.DeleteJob(generateID())
}

// TestRunStore_GetMissingReturnsErrNotExist — fs.ErrNotExist must propagate
// unchanged when the file simply isn't there.
func TestRunStore_GetMissingReturnsErrNotExist(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	_, err := s.Get(generateID(), generateRunID())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Get(missing) err = %v want fs.ErrNotExist", err)
	}
}

// TestRunStore_GetCorruptReturnsErrCorruptRun — a malformed JSON file
// surfaces as ErrCorruptRun.
func TestRunStore_GetCorruptReturnsErrCorruptRun(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := generateID()
	runID := generateRunID()

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
	jobID := generateID()
	runID := generateRunID()

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
	jobID := generateID()

	good := makeRun(jobID, time.Now())
	s.Append(good)

	// Plant a sibling corrupt file.
	dir := filepath.Join(s.root, jobID)
	bad := filepath.Join(dir, generateRunID()+".json")
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

// TestRunStore_ListBeforeCutoff — stagger StartedAt by 1h, ask for entries
// strictly before "now-2h", expect only those satisfying StartedAt < cutoff.
func TestRunStore_ListBeforeCutoff(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := generateID()

	now := time.Now()
	for i := 0; i < 5; i++ {
		// runs[0] = now-4h, runs[1] = now-3h, ... runs[4] = now.
		startedAt := now.Add(-time.Duration(4-i) * time.Hour)
		run := makeRun(jobID, startedAt)
		s.Append(run)
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
	jobID := generateID()

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

	jobIDs := []string{generateID(), generateID(), generateID()}
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

// TestRunStore_RecentReturnsNewestFirst — Recent honours mtime-desc order
// produced by List under the hood.
func TestRunStore_RecentReturnsNewestFirst(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := generateID()

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
