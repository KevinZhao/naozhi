package cron

import (
	"os"
	"strings"
	"testing"
)

// TestRunDeadlineWatchdog_GodocBoundPinned pins R040034-GO-4 (#1390):
// the inner-goroutine accumulation bound MUST stay documented on
// runDeadlineWatchdog. The bound is the operator-facing answer to
// "why is CronWatchdogInterruptTimeoutTotal non-zero?" — losing this
// godoc would force every operator hitting a wedged backend to
// rediscover the persistent-vs-fresh distinction from scratch.
func TestRunDeadlineWatchdog_GodocBoundPinned(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("scheduler_run.go")
	if err != nil {
		t.Fatalf("read scheduler_run.go: %v", err)
	}
	src := string(body)
	if !strings.Contains(src, "Inner-goroutine accumulation bound") {
		t.Fatal("scheduler_run.go missing the runDeadlineWatchdog inner-goroutine bound godoc; do not delete without re-anchoring R040034-GO-4 (#1390)")
	}
	if !strings.Contains(src, "CronWatchdogInterruptTimeoutTotal") {
		t.Fatal("godoc lost the metric anchor that operators alert on")
	}
	if !strings.Contains(src, "fresh-context jobs") || !strings.Contains(src, "persistent-context jobs") {
		t.Fatal("godoc lost the fresh-vs-persistent bound distinction; without it operators can't tell whether the leak is bounded or growing")
	}
}
