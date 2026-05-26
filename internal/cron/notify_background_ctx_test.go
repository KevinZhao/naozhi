package cron

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestNotifyTarget_ChainsToStopCtxContract pins the R243-SEC-14 (#799)
// reversal of the original R38-REL3 contract.
//
// Original R38-REL3 reasoning was: notifyTarget uses context.Background as
// the parent for its 30s Reply timeout because Scheduler.Stop drains in
// two phases (stopCancel first, triggerWG.Wait second), and a Background
// parent kept in-flight replies alive across the cancel edge.
//
// R243-SEC-14 (#799) replaced that with a chain to s.stopCtx: a hung
// webhook would otherwise pin triggerWG.Wait at the full stopBudget
// (30s) waiting for the per-target timer to expire, blocking
// Scheduler.Stop() past systemd TimeoutStopSec. The chained parent lets
// stopCancel short-circuit the Reply call directly so triggerWG.Wait
// drains as soon as the in-flight POST acknowledges the cancel.
//
// The risk this test guards (now): a future "make notify path resilient
// to shutdown again" patch that mechanically reverts to context.Background
// would silently re-introduce the 30s wedge on shutdown. Pin the invariant
// at source level:
//
//  1. notifyTarget's WithTimeout parent is NOT context.Background (the
//     pre-#799 anti-pattern).
//  2. The function references s.stopCtx (or a sibling that derives from
//     it) so the cancel edge propagates.
//  3. The R243-SEC-14 comment tag survives as the in-code anchor.
//
// Any future edit that wires Background back into this path must
// re-argue the #799 shutdown latency budget before passing this test.
func TestNotifyTarget_ChainsToStopCtxContract(t *testing.T) {
	t.Parallel()
	_, thisFile, _, _ := runtime.Caller(0)
	target := filepath.Join(filepath.Dir(thisFile), "scheduler_notify.go")
	src, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read scheduler_notify.go: %v", err)
	}
	body := string(src)

	startIdx := strings.Index(body, "func (s *Scheduler) notifyTarget(")
	if startIdx < 0 {
		t.Fatal("notifyTarget is no longer defined in scheduler_notify.go. " +
			"If it was renamed, update this contract test; if removed, " +
			"R243-SEC-14 trivially closes but the replacement path must " +
			"document its own ctx-ancestry decision.")
	}
	rest := body[startIdx:]
	endRel := regexp.MustCompile(`\nfunc `).FindStringIndex(rest[6:])
	var fnBody string
	if endRel != nil {
		fnBody = rest[:6+endRel[0]]
	} else {
		fnBody = rest
	}

	// 1) Must reference s.stopCtx (directly or via a local that captures it).
	if !strings.Contains(fnBody, "s.stopCtx") {
		t.Error("notifyTarget no longer references s.stopCtx. R243-SEC-14 " +
			"(#799): the parent must chain to s.stopCtx so a hung webhook " +
			"unblocks the moment Scheduler.Stop fires instead of waiting for " +
			"the per-target cronNotifyTimeout (30s). If you need to keep " +
			"replies alive across shutdown again, document the new " +
			"shutdown-latency budget before reverting.")
	}

	// 2) Must NOT use context.Background as the WithTimeout parent — that
	// is the pre-#799 anti-pattern.
	bgCtxRe := regexp.MustCompile(`context\.WithTimeout\(\s*context\.Background\(\)`)
	if bgCtxRe.MatchString(fnBody) {
		t.Error("notifyTarget uses context.WithTimeout(context.Background(), …) " +
			"as the reply ctx parent. R243-SEC-14 (#799): this is the exact " +
			"pre-fix shape that left a hung webhook pinning triggerWG.Wait at " +
			"the full stopBudget. Chain to s.stopCtx instead.")
	}

	// 3) The R243-SEC-14 anchor must survive — it is the in-code reference
	// future readers grep for.
	if !strings.Contains(fnBody, "R243-SEC-14") && !strings.Contains(fnBody, "#799") {
		t.Error("notifyTarget's R243-SEC-14 / #799 anchor comment has been " +
			"removed. The comment is the only in-code reference explaining " +
			"why the parent is s.stopCtx rather than Background. Keep text " +
			"along the lines of \"R243-SEC-14\" or \"#799\" so future audits " +
			"can grep to the reasoning.")
	}
}
