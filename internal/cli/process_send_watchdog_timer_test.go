package cli

import (
	"os"
	"strings"
	"testing"
)

// TestProcessSendNoTicker locks in R246-PERF-3 (#714): the per-Send
// watchdog must use time.NewTimer + Reset (single timer-heap entry per
// turn) rather than time.NewTicker (which spawns a goroutine + chan that
// fires for the whole turn lifetime).
//
// This is a static-source guard — there is no portable way to count
// runtime timer-heap entries from a unit test, but a CI-time grep of
// process_send.go for time.NewTicker catches a revert immediately. The
// allowed pattern is time.NewTimer; if a future contributor needs a
// ticker for a different reason, this test should be updated to
// whitelist that specific call site rather than removed wholesale.
func TestProcessSendNoTicker(t *testing.T) {
	data, err := os.ReadFile("process_send.go")
	if err != nil {
		t.Fatalf("read process_send.go: %v", err)
	}
	src := string(data)
	if strings.Contains(src, "time.NewTicker(") {
		t.Errorf("process_send.go contains time.NewTicker — R246-PERF-3 (#714) requires NewTimer+Reset to avoid per-turn ticker goroutine + chan overhead")
	}
	if !strings.Contains(src, "time.NewTimer(") {
		t.Errorf("process_send.go missing time.NewTimer — watchdog implementation absent")
	}
}
