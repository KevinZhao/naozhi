package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestScanSortedRunDir_SkipSortTrivial pins R260528-PERF-25 (#1361): the
// sort step is skipped when scanSortedRunDir has 0 or 1 surviving items
// (a guaranteed no-op for the sort), so a never-/once-run job's cold
// cacheGet → warmCache path avoids the comparator setup. Correctness must
// be unaffected: the empty dir returns no items, the single-run dir returns
// exactly that item, and a multi-run dir still comes back fully mtime-DESC
// sorted.
func TestScanSortedRunDir_SkipSortTrivial(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 10, 30*24*time.Hour)

	// Empty dir (mkdir but no files) → zero items, no panic from the skip.
	emptyJob := mustGenerateID()
	if err := os.MkdirAll(filepath.Join(s.root, emptyJob), 0o700); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	items, _, err := s.scanSortedRunDir(emptyJob)
	if err != nil {
		t.Fatalf("scan empty: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("empty dir: got %d items want 0", len(items))
	}

	// Single run → exactly one item, returned intact despite the skipped sort.
	oneJob := mustGenerateID()
	dir := filepath.Join(s.root, oneJob)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir one: %v", err)
	}
	wantID := mustGenerateRunID()
	if err := os.WriteFile(filepath.Join(dir, wantID+".json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write one: %v", err)
	}
	items, _, err = s.scanSortedRunDir(oneJob)
	if err != nil {
		t.Fatalf("scan one: %v", err)
	}
	if len(items) != 1 || items[0].runID != wantID {
		t.Fatalf("single run: got %+v want one item runID=%s", items, wantID)
	}

	// Multi run → still fully sorted newest-first (sort path not skipped).
	multiJob := mustGenerateID()
	mdir := filepath.Join(s.root, multiJob)
	if err := os.MkdirAll(mdir, 0o700); err != nil {
		t.Fatalf("mkdir multi: %v", err)
	}
	now := time.Now()
	for i := 0; i < 3; i++ {
		rid := mustGenerateRunID()
		p := filepath.Join(mdir, rid+".json")
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write multi: %v", err)
		}
		// Older index → older mtime, so newest-first means descending index.
		mt := now.Add(time.Duration(-i) * time.Minute)
		_ = os.Chtimes(p, mt, mt)
	}
	items, _, err = s.scanSortedRunDir(multiJob)
	if err != nil {
		t.Fatalf("scan multi: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("multi run: got %d items want 3", len(items))
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].mtime.Before(items[i].mtime) {
			t.Fatalf("multi run not newest-first sorted: %+v", items)
		}
	}
}
