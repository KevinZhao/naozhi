package cron

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestNotifyTarget_UsesBackgroundCtxContract is the R38-REL3 pin for the
// intentional ctx-ancestry deviation in notifyTarget.
//
// Every other long-lived goroutine in the cron package derives its ctx from
// s.stopCtx so Stop()'s cancel edge aborts in-flight work promptly. notifyTarget
// deliberately breaks that rule: it uses context.Background as the parent for
// its 30s Reply timeout because Scheduler.Stop drains in two phases:
//
//  1. s.stopCancel() — cuts all stopCtx-derived work.
//  2. cron.Stop() / triggerWG.Wait() — drains in-flight jobs that may have
//     STARTED before (1) and are now trying to deliver their IM replies.
//
// If notifyTarget used stopCtx, its ReplyWithRetry would bail the moment
// Stop() cancelled the root — any result the scheduler took 10 minutes to
// compute would vanish silently at shutdown. The Background parent with a
// local 30s timeout lets the job finish its reply within the Round 98
// stopBudget (30s total) and still honours shutdown pressure through that
// timeout.
//
// The risk this test guards: a future "unify all ctx derivation" refactor
// that mechanically replaces `context.Background()` with s.stopCtx here
// would silently erase the intent. Pin the invariant at source level:
//
//  1. notifyTarget constructs its ctx via context.WithTimeout(
//     context.Background(), ...).
//  2. The function does NOT reference s.stopCtx.
//  3. The tripwire comment explaining the ancestry deviation survives.
//
// Any future edit that wires stopCtx into this path must re-argue the
// Round 98 stopBudget shutdown timeline before passing this test.
func TestNotifyTarget_UsesBackgroundCtxContract(t *testing.T) {
	src, err := os.ReadFile("scheduler.go")
	if err != nil {
		t.Fatalf("read scheduler.go: %v", err)
	}
	body := string(src)

	// Locate the notifyTarget body.
	startIdx := strings.Index(body, "func (s *Scheduler) notifyTarget(")
	if startIdx < 0 {
		t.Fatal("notifyTarget is no longer defined in scheduler.go. " +
			"If it was renamed, update this contract test; if removed, " +
			"R38-REL3 trivially closes but the replacement path must " +
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

	// 1) Must construct its ctx from context.Background().
	bgCtxRe := regexp.MustCompile(`context\.WithTimeout\(\s*context\.Background\(\)`)
	if !bgCtxRe.MatchString(fnBody) {
		t.Error("notifyTarget no longer derives its ctx from " +
			"context.WithTimeout(context.Background(), ...). R38-REL3: the " +
			"Background parent is deliberate — stopCtx is cancelled BEFORE " +
			"cron.Stop drains in-flight jobs, so wiring stopCtx here would " +
			"silently kill every cron-triggered IM reply at shutdown. " +
			"If you need a cancellable path, pass a dedicated ctx into " +
			"notifyTarget rather than reach up to s.stopCtx.")
	}

	// 2) Must NOT mention s.stopCtx inside the function body — the whole
	// point of this audit item is that the ancestry deviation stays
	// explicit. A direct `s.stopCtx` reference would re-open the same bug
	// the godoc warns about.
	if strings.Contains(fnBody, "s.stopCtx") {
		t.Error("notifyTarget references s.stopCtx. R38-REL3: the Background " +
			"parent ancestry is intentional. Thread any needed cancellation " +
			"through an explicit parameter instead — this keeps the " +
			"\"replies survive shutdown cancel but die on 30s timeout\" " +
			"contract readable at the call site.")
	}

	// 3) The explanatory comment tag must survive — it anchors the godoc
	// to future readers.
	if !strings.Contains(fnBody, "stopCtx is cancelled first") &&
		!strings.Contains(fnBody, "Use Background parent") {
		t.Error("notifyTarget's ctx-ancestry-deviation comment has been " +
			"removed. R38-REL3: the comment is the only in-code anchor " +
			"explaining why this Background parent is not an oversight. " +
			"Keep text along the lines of \"Use Background parent\" or " +
			"\"stopCtx is cancelled first\" so a search for either phrase " +
			"lands on the reasoning.")
	}
}
