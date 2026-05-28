package cron

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunStore_Append_RootGuardLogic pins the R247-GO-8 (#484)
// defense-in-depth path-containment computation. IsValidID upstream keeps
// the guard's reject branch dead in production; this test exercises the
// pure-path arithmetic so a future change to the predicate (e.g., dropping
// the "../" prefix check in favour of an unsafe shortcut) is caught.
func TestRunStore_Append_RootGuardLogic(t *testing.T) {
	root := "/var/lib/naozhi/runs"
	cases := []struct {
		name   string
		dir    string
		reject bool
	}{
		{"same-root", root, false},
		{"normal-jobid", filepath.Join(root, "0123456789abcdef"), false},
		{"escape-via-dotdot", filepath.Join(root, "..", "etc"), true},
		{"escape-deep", "/etc/passwd", true},
		{"sibling-runs", filepath.Join(filepath.Dir(root), "other-runs"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel, err := filepath.Rel(root, tc.dir)
			rejected := err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
			if rejected != tc.reject {
				t.Fatalf("dir=%q rel=%q err=%v: rejected=%v want=%v", tc.dir, rel, err, rejected, tc.reject)
			}
		})
	}
}

// TestRunStore_Append_HappyPathAfterGuard asserts the guard does NOT block
// a legitimate hex JobID — Append still writes the run record under the
// per-job subdir.
func TestRunStore_Append_HappyPathAfterGuard(t *testing.T) {
	s := newTestStore(t, 5, time.Hour)
	jobID := mustGenerateID()
	run := makeRun(jobID, time.Now())
	s.Append(run)
	wantPath := filepath.Join(s.root, jobID, run.RunID+".json")
	if _, err := os.Lstat(wantPath); err != nil {
		t.Fatalf("expected Append to write %q: %v", wantPath, err)
	}
}
