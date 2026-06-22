package runhistory

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

func mkRun(t *testing.T, key string, dur int64, started time.Time, oc Outcome) SessionRun {
	t.Helper()
	id, err := NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	return SessionRun{
		RunID:      id,
		SessionKey: key,
		StartedAt:  started,
		EndedAt:    started.Add(time.Duration(dur) * time.Millisecond),
		DurationMS: dur,
		Outcome:    oc,
	}
}

func TestStore_AppendThenRecent_NewestFirst(t *testing.T) {
	st := NewStore(t.TempDir(), 0, 0)
	base := time.Now()
	const key = "feishu:p2p:alice"
	for i := 0; i < 3; i++ {
		st.Append(mkRun(t, key, int64(100+i), base.Add(time.Duration(i)*time.Second), OutcomeCompleted))
	}
	got := st.Recent(key, 0)
	if len(got) != 3 {
		t.Fatalf("want 3 runs, got %d", len(got))
	}
	// newest-first: last appended (i=2) is at index 0
	if got[0].DurationMS != 102 || got[2].DurationMS != 100 {
		t.Errorf("not newest-first: %v", []int64{got[0].DurationMS, got[1].DurationMS, got[2].DurationMS})
	}
}

func TestStore_Persistence_ColdReadAfterRestart(t *testing.T) {
	dir := t.TempDir()
	const key = "feishu:group:team"
	st1 := NewStore(dir, 0, 0)
	st1.Append(mkRun(t, key, 200, time.Now(), OutcomeCompleted))

	// Fresh store over the same dir: must warm from disk.
	st2 := NewStore(dir, 0, 0)
	got := st2.Recent(key, 0)
	if len(got) != 1 || got[0].DurationMS != 200 {
		t.Fatalf("cold read failed: %+v", got)
	}
}

func TestStore_NoIndexJSON(t *testing.T) {
	dir := t.TempDir()
	const key = "weixin:p2p:bob"
	st := NewStore(dir, 0, 0)
	st.Append(mkRun(t, key, 50, time.Now(), OutcomeCompleted))

	root := filepath.Join(dir, "session-runs")
	if _, err := os.Stat(filepath.Join(root, "index.json")); !os.IsNotExist(err) {
		t.Errorf("index.json must not be written (cron-parity), stat err=%v", err)
	}
	// exactly one per-run file under the hashed dir
	hash := dirHashFor(key)
	ents, _ := os.ReadDir(filepath.Join(root, hash))
	if len(ents) != 1 {
		t.Errorf("want 1 run file, got %d", len(ents))
	}
}

func TestStore_KeepCountTrim_EvictsDisk(t *testing.T) {
	dir := t.TempDir()
	const key = "feishu:p2p:carol"
	st := NewStore(dir, 3, 0) // keepCount=3
	base := time.Now()
	for i := 0; i < 6; i++ {
		st.Append(mkRun(t, key, int64(i), base.Add(time.Duration(i)*time.Second), OutcomeCompleted))
	}
	got := st.Recent(key, 0)
	if len(got) != 3 {
		t.Fatalf("ring not trimmed to 3, got %d", len(got))
	}
	// on-disk files also trimmed to 3
	ents, _ := os.ReadDir(filepath.Join(dir, "session-runs", dirHashFor(key)))
	if len(ents) != 3 {
		t.Errorf("disk not trimmed to 3, got %d files", len(ents))
	}
	// the 3 survivors are the newest (durations 5,4,3)
	if got[0].DurationMS != 5 || got[2].DurationMS != 3 {
		t.Errorf("wrong survivors: %d..%d", got[0].DurationMS, got[2].DurationMS)
	}
}

func TestStore_KeepWindow_AgesOutOnWarm(t *testing.T) {
	dir := t.TempDir()
	const key = "feishu:p2p:dave"
	st1 := NewStore(dir, 0, time.Hour)
	// one stale (2h ago), one fresh
	st1.Append(mkRun(t, key, 10, time.Now().Add(-2*time.Hour), OutcomeCompleted))
	st1.Append(mkRun(t, key, 20, time.Now(), OutcomeCompleted))

	// new store warms from disk applying the keepWindow filter
	st2 := NewStore(dir, 0, time.Hour)
	got := st2.Recent(key, 0)
	if len(got) != 1 || got[0].DurationMS != 20 {
		t.Fatalf("stale run not aged out on warm: %+v", got)
	}
}

func TestStore_KeepWindow_DeletesExpiredFilesOnWarm(t *testing.T) {
	// #2225: expired run JSON must be removed from disk on warm, not merely
	// filtered out of the ring — otherwise stale records for slow/removed
	// sessions accumulate unbounded.
	dir := t.TempDir()
	const key = "feishu:p2p:zoe"
	st1 := NewStore(dir, 0, time.Hour)
	// two stale (>1h ago), one fresh
	st1.Append(mkRun(t, key, 10, time.Now().Add(-3*time.Hour), OutcomeCompleted))
	st1.Append(mkRun(t, key, 11, time.Now().Add(-2*time.Hour), OutcomeCompleted))
	st1.Append(mkRun(t, key, 20, time.Now(), OutcomeCompleted))

	hashDir := filepath.Join(dir, "session-runs", dirHashFor(key))
	if ents, _ := os.ReadDir(hashDir); len(ents) != 3 {
		t.Fatalf("precondition: want 3 files on disk, got %d", len(ents))
	}

	// Fresh store warms from disk, applying the keepWindow filter + GC.
	st2 := NewStore(dir, 0, time.Hour)
	if got := st2.Recent(key, 0); len(got) != 1 || got[0].DurationMS != 20 {
		t.Fatalf("stale runs not aged out of ring: %+v", got)
	}
	// The two expired files must be gone from disk; only the fresh one remains.
	ents, _ := os.ReadDir(hashDir)
	if len(ents) != 1 {
		t.Fatalf("expired files not GC'd from disk: want 1, got %d", len(ents))
	}
}

func TestStore_NegativeDurationClamped(t *testing.T) {
	st := NewStore(t.TempDir(), 0, 0)
	const key = "feishu:p2p:eve"
	r := mkRun(t, key, 0, time.Now(), OutcomeCompleted)
	r.DurationMS = -5
	st.Append(r)
	got := st.Recent(key, 0)
	if len(got) != 1 || got[0].DurationMS != 0 {
		t.Fatalf("negative duration not clamped: %+v", got)
	}
}

func TestStore_PathTraversalKeyContained(t *testing.T) {
	dir := t.TempDir()
	key := "../../../etc/passwd:p2p:x"
	st := NewStore(dir, 0, 0)
	st.Append(mkRun(t, key, 1, time.Now(), OutcomeCompleted))
	// everything must land under session-runs/<hash>/, never escape root
	root := filepath.Join(dir, "session-runs")
	hash := dirHashFor(key)
	if _, err := os.Stat(filepath.Join(root, hash)); err != nil {
		t.Fatalf("hashed dir missing: %v", err)
	}
	// no traversal artifact outside root
	if _, err := os.Stat(filepath.Join(dir, "etc")); !os.IsNotExist(err) {
		t.Errorf("path traversal escaped store root")
	}
}

func TestStore_Invalidate_DropsRingKeepsDisk(t *testing.T) {
	dir := t.TempDir()
	const key = "feishu:p2p:frank"
	st := NewStore(dir, 0, 0)
	st.Append(mkRun(t, key, 99, time.Now(), OutcomeCompleted))
	st.Invalidate(key)
	// ring rebuilt from disk -> record still there
	got := st.Recent(key, 0)
	if len(got) != 1 || got[0].DurationMS != 99 {
		t.Fatalf("disk record lost after invalidate: %+v", got)
	}
}

func TestStore_DisabledIsNoop(t *testing.T) {
	st := NewStore("", 0, 0)
	st.Append(mkRun(t, "k:p2p:x", 1, time.Now(), OutcomeCompleted))
	if got := st.Recent("k:p2p:x", 0); got != nil {
		t.Errorf("disabled store should return nil, got %v", got)
	}
	var nilStore *Store
	nilStore.Append(SessionRun{}) // must not panic
	if nilStore.Recent("x", 0) != nil {
		t.Error("nil store Recent should be nil")
	}
}

func TestStore_List_PaginationBefore(t *testing.T) {
	st := NewStore(t.TempDir(), 0, 0)
	const key = "feishu:p2p:gina"
	base := time.Now()
	for i := 0; i < 5; i++ {
		st.Append(mkRun(t, key, int64(i), base.Add(time.Duration(i)*time.Minute), OutcomeCompleted))
	}
	all := st.List(key, 0, time.Time{})
	if len(all) != 5 {
		t.Fatalf("want 5, got %d", len(all))
	}
	// before the 3rd-newest start time -> only older ones
	pivot := all[2].StartedAt
	older := st.List(key, 0, pivot)
	for _, r := range older {
		if !r.StartedAt.Before(pivot) {
			t.Errorf("List(before) returned a run not strictly before pivot")
		}
	}
	// limit cap
	if got := st.List(key, 2, time.Time{}); len(got) != 2 {
		t.Errorf("limit=2 ignored, got %d", len(got))
	}
}

func TestStore_ConcurrentAppend(t *testing.T) {
	st := NewStore(t.TempDir(), 100, 0)
	const key = "feishu:p2p:harry"
	var wg sync.WaitGroup
	base := time.Now()
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st.Append(mkRun(t, key, int64(i), base.Add(time.Duration(i)*time.Millisecond), OutcomeCompleted))
		}(i)
	}
	wg.Wait()
	if got := st.Recent(key, 0); len(got) != 50 {
		t.Errorf("concurrent appends lost records: got %d/50", len(got))
	}
}

func TestComputeStats(t *testing.T) {
	runs := []SessionRun{
		{DurationMS: 100, Outcome: OutcomeCompleted},
		{DurationMS: 200, Outcome: OutcomeCompleted},
		{DurationMS: 300, Outcome: OutcomeError},
		{DurationMS: 400, Outcome: OutcomeTimeout},
	}
	st := ComputeStats(runs)
	if st.Count != 4 || st.TotalMS != 1000 || st.AvgMS != 250 || st.MaxMS != 400 {
		t.Errorf("basic stats wrong: %+v", st)
	}
	if st.CompletedCnt != 2 || st.ErrorCnt != 1 || st.TimeoutCnt != 1 {
		t.Errorf("outcome counts wrong: %+v", st)
	}
	// nearest-rank: P50 of [100,200,300,400] rank=ceil(.5*4)=2 -> 200
	if st.P50MS != 200 {
		t.Errorf("P50 want 200, got %d", st.P50MS)
	}
}

func TestComputeStats_Empty(t *testing.T) {
	if st := ComputeStats(nil); st != (SessionRunStats{}) {
		t.Errorf("empty stats should be zero value, got %+v", st)
	}
}

func TestComputeStats_Single(t *testing.T) {
	st := ComputeStats([]SessionRun{{DurationMS: 42, Outcome: OutcomeCompleted}})
	if st.P50MS != 42 || st.AvgMS != 42 || st.MaxMS != 42 {
		t.Errorf("single-element stats wrong: %+v", st)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantOc  Outcome
		wantCls runtelemetry.ErrorClass
	}{
		{"nil", nil, OutcomeCompleted, runtelemetry.ErrClassNone},
		{"total-timeout", cli.ErrTotalTimeout, OutcomeTimeout, runtelemetry.ErrClassDeadlineExceeded},
		{"no-output-timeout", cli.ErrNoOutputTimeout, OutcomeTimeout, runtelemetry.ErrClassDeadlineExceeded},
		{"ctx-deadline", context.DeadlineExceeded, OutcomeTimeout, runtelemetry.ErrClassDeadlineExceeded},
		{"ctx-canceled", context.Canceled, OutcomeCanceled, runtelemetry.ErrClassCanceled},
		{"other", os.ErrPermission, OutcomeError, runtelemetry.ErrClassNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oc, cls := Classify(tt.err)
			if oc != tt.wantOc || cls != tt.wantCls {
				t.Errorf("Classify(%v) = (%s,%s), want (%s,%s)", tt.err, oc, cls, tt.wantOc, tt.wantCls)
			}
		})
	}
}

func TestStore_AppendAsync_FlushedOnClose(t *testing.T) {
	dir := t.TempDir()
	const key = "feishu:p2p:ivan"
	st := NewStore(dir, 0, 0)
	for i := 0; i < 5; i++ {
		st.AppendAsync(mkRun(t, key, int64(i), time.Now().Add(time.Duration(i)*time.Second), OutcomeCompleted))
	}
	st.Close() // must flush queued writes before returning

	// fresh store reads what the worker persisted
	st2 := NewStore(dir, 0, 0)
	defer st2.Close()
	if got := st2.Recent(key, 0); len(got) != 5 {
		t.Errorf("async records not flushed on close: got %d/5", len(got))
	}
}

func TestStore_AppendAsync_AfterCloseNoPanic(t *testing.T) {
	st := NewStore(t.TempDir(), 0, 0)
	st.Close()
	// must not panic on send to closed channel
	st.AppendAsync(mkRun(t, "feishu:p2p:j", 1, time.Now(), OutcomeCompleted))
	st.Close() // idempotent
}

func TestStore_AppendAsync_DisabledNoop(t *testing.T) {
	st := NewStore("", 0, 0)
	st.AppendAsync(mkRun(t, "k:p2p:x", 1, time.Now(), OutcomeCompleted))
	st.Close()
	if st.DropTotal() != 0 {
		t.Errorf("disabled store should not count drops")
	}
}

func TestNewRunID_ValidHex(t *testing.T) {
	id, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 16 || !isValidRunID(id) {
		t.Errorf("bad run id %q", id)
	}
}
