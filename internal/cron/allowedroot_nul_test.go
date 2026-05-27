package cron

import (
	"strings"
	"testing"
)

// TestNewScheduler_AllowedRootNULBytePruned covers R20260527-PERF-14 (#1297).
// An operator-set NUL byte in cfg.AllowedRoot would corrupt the precomputed
// workDirCacheKeySuffix ("\x00" + AllowedRoot + "\x00" + allowedRootResolved)
// because NUL is the field separator in that key. NewScheduler must strip
// the misconfigured AllowedRoot at construction so subsequent workDir
// resolution either succeeds with no root constraint or rejects all work
// dirs by the empty-root branch — never aliases an attacker-influenced
// path onto an unrelated cache slot.
func TestNewScheduler_AllowedRootNULBytePruned(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{
		AllowedRoot:    "/tmp/safe\x00/etc",
		MaxJobs:        5,
		AllowNilRouter: true,
	})
	if s.allowedRoot != "" {
		t.Fatalf("allowedRoot = %q, want empty (NUL byte should be rejected)", s.allowedRoot)
	}
	if strings.ContainsRune(s.workDirCacheKeySuffix, 0) {
		// "\x00\x00" (empty AllowedRoot + empty allowedRootResolved) is
		// the legitimate two-NUL suffix, but neither half should have
		// embedded the user-supplied NUL.
		// The legit pattern is exactly "\x00\x00".
		if s.workDirCacheKeySuffix != "\x00\x00" {
			t.Errorf("workDirCacheKeySuffix = %q, expected exact \"\\x00\\x00\" with no embedded user NUL", s.workDirCacheKeySuffix)
		}
	}
}
