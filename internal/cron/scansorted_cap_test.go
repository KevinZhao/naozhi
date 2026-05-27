package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestScanSortedRunDir_CapBoundedByKeepCount pins the R249-PERF-25 (#940)
// fix that scanSortedRunDir's `items` slice initial capacity is bounded
// by 2*keepCount even when the directory contains many filtered entries
// (.tmp orphans, hidden dotfiles, non-hex names). Pre-fix the cap was
// `len(entries)` — every filtered entry paid an alloc slot that would
// never be used.
//
// Test shape: build runs/<jobID>/ with keepCount=4 hex .json files plus
// 100 non-json orphan files. After scanSortedRunDir, the returned slice
// holds only the 4 valid items, AND its underlying capacity should be
// ≤ 2*keepCount (= 8) — the new bound — rather than ≥ 100 + 4 (= 104),
// the historical bound. Capacity is observable via cap() on the
// returned slice.
//
// We assert correctness (filtered count) AND the capacity bound, so a
// future refactor that loses either property fails the test
// independently. Directly synthesising the directory contents (not via
// Append) is intentional: Append refuses non-hex names at the rune
// level so the orphan rows could not be created via the public API.
func TestScanSortedRunDir_CapBoundedByKeepCount(t *testing.T) {
	t.Parallel()
	const keepCount = 4
	const orphanCount = 100

	s := newTestStore(t, keepCount, 30*24*time.Hour)
	jobID := mustGenerateID()
	dir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Drop keepCount valid run files (hex name + .json) so scanSortedRunDir
	// has real items to return.
	now := time.Now()
	for i := 0; i < keepCount; i++ {
		runID := mustGenerateRunID()
		path := filepath.Join(dir, runID+".json")
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write run: %v", err)
		}
		// Stagger mtime so the sort step has work to do, but pin to
		// distinct values so a sorted-correctness regression would also
		// surface — orthogonal to the cap bound this test pins.
		mt := now.Add(time.Duration(i) * time.Second)
		_ = os.Chtimes(path, mt, mt)
	}

	// Drop orphanCount non-json / non-hex / dotfile entries that the
	// filter loop must skip without keeping in `items`.
	for i := 0; i < orphanCount; i++ {
		// Mix of orphan shapes the historical cap=len(entries) would
		// have over-allocated for: .tmp atomic-write debris, hidden
		// dotfiles, plain text scratch.
		var name string
		switch i % 3 {
		case 0:
			name = mustGenerateRunID() + ".tmp"
		case 1:
			name = ".hidden" + mustGenerateRunID()
		default:
			name = "scratch" + mustGenerateRunID() + ".txt"
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("orphan"), 0o600); err != nil {
			t.Fatalf("write orphan: %v", err)
		}
	}

	items, _, err := s.scanSortedRunDir(jobID)
	if err != nil {
		t.Fatalf("scanSortedRunDir: %v", err)
	}
	if got, want := len(items), keepCount; got != want {
		t.Fatalf("len(items) = %d, want %d (orphans should be filtered)", got, want)
	}
	// Cap bound: 2*keepCount per the R249-PERF-25 fix. Prior code would
	// have returned cap == len(entries) == keepCount + orphanCount = 104.
	maxCap := 2 * keepCount
	if got := cap(items); got > maxCap {
		t.Fatalf("cap(items) = %d, want ≤ %d (R249-PERF-25 / #940 bound)", got, maxCap)
	}
}

// TestScanSortedRunDir_CapKeepCountZeroFallsBackToLen pins that when
// keepCount is zero (disabled / mis-configured store) the cap falls
// back to len(entries) so we don't degrade the well-formed steady-state
// case to many-realloc growth. The newRunStore constructor normally
// substitutes the default keepCount when caller passes ≤0, so this is
// a defence-in-depth test against a future direct-construction path.
func TestScanSortedRunDir_CapKeepCountZeroFallsBackToLen(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "runs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s := &runStore{root: root, keepCount: 0, maxRunBytes: MaxRunRecordBytes}
	jobID := mustGenerateID()
	dir := filepath.Join(root, jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := 0; i < 5; i++ {
		runID := mustGenerateRunID()
		path := filepath.Join(dir, runID+".json")
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	items, _, err := s.scanSortedRunDir(jobID)
	if err != nil {
		t.Fatalf("scanSortedRunDir: %v", err)
	}
	if got := len(items); got != 5 {
		t.Fatalf("len(items) = %d want 5", got)
	}
	// keepCount=0 → cap fallback to len(entries)=5; this ensures the
	// guard `if s.keepCount > 0` path is exercised.
	if got := cap(items); got != 5 {
		t.Fatalf("cap(items) = %d want 5 (keepCount=0 fallback)", got)
	}
}
