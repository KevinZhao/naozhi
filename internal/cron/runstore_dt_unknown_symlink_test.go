package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunStore_ScanSortedRunDir_SkipsSymlinkViaInfoMode covers the
// R236-SEC-04 (#489) DT_UNKNOWN bypass: even if e.Type() reports 0 (as
// some FUSE / tmpfs / NFS implementations do), info.Mode() from e.Info()
// is the authoritative kind. The test seeds a runs/<jobID>/ dir with one
// real .json file and a symlink-to-elsewhere with the same .json suffix
// and a hex name. scanSortedRunDir must return the real file and skip the
// symlink — the e.Type() ModeSymlink check still fires on Linux ext4 (so
// this test is dominated by that path on most CI), but the assertion
// pins that the post-Info() recheck does NOT erroneously reject the
// real file, which is the safety property the new guard adds.
func TestRunStore_ScanSortedRunDir_SkipsSymlinkViaInfoMode(t *testing.T) {
	s := newTestStore(t, 5, time.Hour)
	jobID := mustGenerateID()
	dir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}

	// Real run file.
	realRun := makeRun(jobID, time.Now())
	s.Append(realRun)

	// Plant a symlink with hex .json name pointing outside the runs tree.
	target := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(target, []byte(`{"job_id":"x"}`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	symPath := filepath.Join(dir, mustGenerateRunID()+".json")
	if err := os.Symlink(target, symPath); err != nil {
		t.Skipf("symlink unsupported on this fs: %v", err)
	}

	items, _, err := s.scanSortedRunDir(jobID)
	if err != nil {
		t.Fatalf("scanSortedRunDir: %v", err)
	}
	for _, it := range items {
		if it.path == symPath {
			t.Fatalf("scanSortedRunDir leaked symlink %q in items %+v", symPath, items)
		}
	}
	// Real file MUST still be present — the new info.Mode() recheck must
	// not reject regular files (it filters !IsRegular() — the real file
	// is regular, so it passes).
	wantPath := filepath.Join(dir, realRun.RunID+".json")
	found := false
	for _, it := range items {
		if it.path == wantPath {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("scanSortedRunDir dropped legitimate file %q (items=%+v)", wantPath, items)
	}
}
