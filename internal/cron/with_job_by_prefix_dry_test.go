package cron

import (
	"os"
	"strings"
	"testing"
)

// TestWithJobByPrefix_CollapsesDeletePauseResume pins R247-CR-2 (#583): the
// IM-prefix mutation entry points (DeleteJob / PauseJob / ResumeJob) must
// route through a single withJobByPrefix helper — mirroring the *ByID twin
// at withJobByID — instead of carrying three open-coded copies of
// "lock → findByPrefix → mutate → persist → unlock → side-effect" (~150
// LOC pre-#743).
//
// Structural pin rather than a behavioural assertion: the three callers
// today are 1-3 line wrappers. A future maintainer who adds a fourth
// IM-prefix mutator (e.g. RenameJob) by copy-pasting the original 25-line
// body would silently re-introduce the duplication this issue chases. The
// pin guards the helper's existence + its three call sites so a revert is
// a build-time test failure rather than a code-review-only catch.
func TestWithJobByPrefix_CollapsesDeletePauseResume(t *testing.T) {
	src, err := os.ReadFile("scheduler_jobs_prefix.go")
	if err != nil {
		t.Fatalf("read scheduler_jobs_prefix.go: %v", err)
	}
	body := string(src)

	// 1. The helper itself must exist with the documented signature shape.
	const sig = "func (s *Scheduler) withJobByPrefix("
	if !strings.Contains(body, sig) {
		t.Fatalf("withJobByPrefix helper missing — R247-CR-2 / #583 collapse " +
			"reverted. The IM-prefix Delete/Pause/Resume entry points must " +
			"share one 3-phase frame (lock+find+op / persist / unlock+" +
			"postCleanup) so future mutators add a 1-line wrapper rather " +
			"than another 25-line twin.")
	}

	// 2. All three IM-prefix mutators must route through the helper.
	for _, fn := range []string{
		"func (s *Scheduler) DeleteJob(",
		"func (s *Scheduler) PauseJob(",
		"func (s *Scheduler) ResumeJob(",
	} {
		idx := strings.Index(body, fn)
		if idx < 0 {
			t.Fatalf("entry point %q missing from scheduler_jobs_prefix.go", fn)
		}
		// Pull the function body up to the next "\nfunc " marker.
		rest := body[idx:]
		if next := strings.Index(rest[len(fn):], "\nfunc "); next >= 0 {
			rest = rest[:len(fn)+next]
		}
		if !strings.Contains(rest, "withJobByPrefix(") {
			t.Errorf("%s no longer delegates to withJobByPrefix — R247-CR-2 "+
				"/ #583 regression. The 3 prefix-by-chat mutators must "+
				"share the helper or the ~150-LOC duplication returns.",
				strings.TrimPrefix(fn, "func (s *Scheduler) "))
		}
	}

	// 3. The locked find → op → persist phase must live inside the shared
	// helper layer (lockedJobPrefixOp, extracted from withJobByPrefix's IIFE
	// in R249-CR-7 / #951) so the 3-phase contract is observable from the
	// source. This catches a partial revert that inlines the lookup/persist
	// back into each caller. withJobByPrefix must delegate to
	// lockedJobPrefixOp, and lockedJobPrefixOp must own the find + persist.
	{
		idx := strings.Index(body, sig)
		rest := body[idx:]
		if next := strings.Index(rest[len(sig):], "\nfunc "); next >= 0 {
			rest = rest[:len(sig)+next]
		}
		if !strings.Contains(rest, "lockedJobPrefixOp(") {
			t.Error("withJobByPrefix must delegate to lockedJobPrefixOp — " +
				"R249-CR-7 / #951 moved the s.mu critical section into that " +
				"named helper; inlining it back undoes the IIFE cleanup.")
		}

		const opSig = "func (s *Scheduler) lockedJobPrefixOp("
		opIdx := strings.Index(body, opSig)
		if opIdx < 0 {
			t.Fatalf("lockedJobPrefixOp helper missing — R249-CR-7 / #951 " +
				"extraction reverted.")
		}
		opRest := body[opIdx:]
		if next := strings.Index(opRest[len(opSig):], "\nfunc "); next >= 0 {
			opRest = opRest[:len(opSig)+next]
		}
		if !strings.Contains(opRest, "findByPrefixLocked(") {
			t.Error("lockedJobPrefixOp must call findByPrefixLocked under " +
				"s.mu — moving the lookup back into callers undoes the DRY " +
				"collapse R247-CR-2 / #583 chases.")
		}
		if !strings.Contains(opRest, "persistJobsLocked()") {
			t.Error("lockedJobPrefixOp must call persistJobsLocked() so the " +
				"persist phase stays inside the helper rather than spread " +
				"across the 3 callers.")
		}
	}
}
