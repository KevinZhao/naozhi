package cli

import (
	"os"
	"strings"
	"testing"
)

// TestProcessSend_WatchdogIsTimerNotTicker pins R246-PERF-3 (#714): the
// per-Send watchdog must be a single time.Timer that we Stop+Reset, not
// time.NewTicker. NewTicker on the hot Send path leaks a backing goroutine +
// channel per user message until the next GC round; under a sustained
// dispatch storm this caused observable runtime allocation and timer-heap
// pressure. The fix landed as time.NewTimer + watchdog.Reset(checkInterval)
// on each fire — re-introducing time.NewTicker here would silently regress
// the optimisation.
//
// We assert via source-text inspection because there is no public hook to
// observe timer kind at runtime; behavioural tests of the watchdog already
// cover the fire / reset logic. A grep-style check is the cheapest pin
// short of a custom go/ast scan.
func TestProcessSend_WatchdogIsTimerNotTicker(t *testing.T) {
	src, err := os.ReadFile("process_send.go")
	if err != nil {
		t.Fatalf("read process_send.go: %v", err)
	}
	body := string(src)

	if strings.Contains(body, "time.NewTicker(") {
		t.Errorf("process_send.go uses time.NewTicker — R246-PERF-3 / #714 regressed; expected time.NewTimer + Reset")
	}
	if !strings.Contains(body, "time.NewTimer(") {
		t.Errorf("process_send.go missing time.NewTimer — watchdog implementation drifted away from R246-PERF-3 / #714")
	}
	if !strings.Contains(body, "watchdog.Reset(checkInterval)") {
		t.Errorf("process_send.go missing watchdog.Reset(checkInterval) — re-arm path required for the Timer to behave like a ticker; see R246-PERF-3 / #714")
	}
}
