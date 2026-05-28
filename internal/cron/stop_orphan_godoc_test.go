package cron

import (
	"os"
	"regexp"
	"testing"
)

// TestStopGodoc_EnumeratesAllThreeOrphanSites pins R236-GO-05 (#498)
// and the related #1072 doc-anchor decision. Stop()'s godoc must
// explicitly call out all three intentional-orphan goroutine sites so
// an operator reading post-Stop log lines like "deadline exceeded
// during triggerWG wait" can map the warning back to a documented
// design choice instead of suspecting a fresh leak.
//
// The three sites are:
//  1. drainTriggerWG's `go func() { triggerWG.Wait(); close(...) }()`
//     wrapper — the underlying triggerWG.Wait has no cancel signal,
//     so a stuck deliverNotice goroutine pins this wrapper until the
//     OS reclaims the process.
//  2. waitGCDrain's `go func() { gcWG.Wait(); close(...) }()` wrapper
//     — same shape for a stuck trimAll on a wedged ReadDir/Remove.
//  3. runDeadlineWatchdog (in scheduler_run.go) — already pinned by
//     stop_watchdog_orphan_doc_test.go's TestStopGodoc_EnumeratesWatchdogOrphan.
//
// #498's proposal (add scheduler.stopCh, select on it in TriggerNow
// goroutines) is equivalent to the current stopCtx propagation: the
// jitter/spawn/send paths already short-circuit on stopCtx-derived
// contexts, and notifyTarget's replyCtx is parented on s.stopCtx.
// Adding stopCh would observe the same cancel edge with no behaviour
// delta. Option (a) — doc-anchor the intentional-orphan contract — is
// the chosen remediation for all three sites; this test pins the docs.
//
// A future change that quietly drops either the triggerWG or gcWG
// rationale block while keeping just the watchdog block would let an
// operator confronted with "stop deadline exceeded" treat it as a
// fresh leak. This test fails first if any of the three rationales
// disappears from Stop's godoc.
func TestStopGodoc_EnumeratesAllThreeOrphanSites(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("scheduler.go")
	if err != nil {
		t.Fatalf("read scheduler.go: %v", err)
	}
	body := string(src)

	idxStop := regexp.MustCompile(`func\s+\(\s*s\s+\*Scheduler\s*\)\s+Stop\s*\(`).FindStringIndex(body)
	if idxStop == nil {
		t.Fatalf("scheduler.go: Stop method definition not found")
	}
	preamble := body[:idxStop[0]]

	cases := []struct {
		name    string
		pattern *regexp.Regexp
		issue   string
	}{
		{
			name:    "triggerWG_orphan",
			pattern: regexp.MustCompile(`triggerWG\.Wait`),
			issue:   "#498 / R236-GO-05",
		},
		{
			name:    "gcWG_orphan",
			pattern: regexp.MustCompile(`gcWG\.Wait`),
			issue:   "#498 / R247-GO-7",
		},
		{
			name:    "watchdog_orphan",
			pattern: regexp.MustCompile(`runDeadlineWatchdog`),
			issue:   "#1072 / R250-GO-9",
		},
		{
			name:    "intentional_orphan_phrase",
			pattern: regexp.MustCompile(`(?i)intentional-orphan`),
			issue:   "#498 / #1072",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.pattern.MatchString(preamble) {
				t.Errorf("scheduler.go Stop godoc missing %q rationale (issue %s); "+
					"the doc-anchor remediation requires all three orphan sites to stay enumerated",
					tc.pattern.String(), tc.issue)
			}
		})
	}
}
