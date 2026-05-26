package cron

import (
	"os"
	"strings"
	"testing"
)

// TestExecuteJobIDIfLive_PanicRecoverPresent is a source-level regression
// for issue #801 (R238-GO-9): the TriggerNow dispatch path bypasses the
// robfig/cron chain.Recover wrapper, so executeJobIDIfLive must contain
// its own `defer ... recover()` to keep an executeOpt panic from killing
// the host process via the triggerWG goroutine.
//
// Source-level inspection is sufficient — the recover sits at the very
// top of the function before any other work, so any tampering will be
// visible in the file. Behavioural fault-injection would require a
// session.Send hook the cron package does not yet expose.
func TestExecuteJobIDIfLive_PanicRecoverPresent(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("scheduler_run.go")
	if err != nil {
		t.Fatalf("read scheduler_run.go: %v", err)
	}
	body := string(src)

	header := "func (s *Scheduler) executeJobIDIfLive("
	idx := strings.Index(body, header)
	if idx < 0 {
		t.Fatalf("executeJobIDIfLive not found in scheduler_run.go")
	}
	// Look at the function body up to its closing brace. The recover
	// defer must be before any work that could panic (snapshot read,
	// executeOpt call). 2000 bytes is generous: the function is < 50
	// lines.
	end := idx + 2000
	if end > len(body) {
		end = len(body)
	}
	prelude := body[idx:end]

	if !strings.Contains(prelude, "recover()") {
		t.Errorf("executeJobIDIfLive must defer a recover() to absorb "+
			"executeOpt panics on the TriggerNow path (issue #801). "+
			"Prelude:\n%s", prelude)
	}
	if !strings.Contains(prelude, "defer func()") {
		t.Errorf("executeJobIDIfLive's recover must live inside an " +
			"anonymous defer so it fires before the goroutine unwinds")
	}
	// recover must come before the s.executeOpt call.
	recIdx := strings.Index(prelude, "recover()")
	execIdx := strings.Index(prelude, "s.executeOpt(")
	if recIdx < 0 || execIdx < 0 || recIdx > execIdx {
		t.Errorf("recover() must be installed before s.executeOpt is "+
			"called; got recIdx=%d execIdx=%d", recIdx, execIdx)
	}
}
