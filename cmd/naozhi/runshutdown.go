// File: runshutdown.go
//
// Graceful-shutdown teardown sequence, extracted from main()'s runShutdown
// closure so the sysMgr → scheduler → http-drain → router ordering becomes a
// testable structure rather than a hand-rolled straight-line of statements
// inside a closure (R20260530-ARCH-3 / #1487, R260528-ARCH-15 / #1376).
//
// The order is a hard correctness contract, NOT a topo-sort-derivable one:
//   - sysMgr.Stop must run FIRST: daemon Tick paths call into the router
//     (VisitSessions / SetUserLabelWithOrigin); leaving them running while
//     downstream state tears down would race.
//   - scheduler.Stop must complete before router teardown: in-flight cron
//     jobs still call GetOrCreate/Send on the router.
//   - the HTTP drain barrier must clear before router.Shutdown so no handler
//     observes a half-cleaned session map.
//   - router.Shutdown runs LAST.
//
// Previously this ordering lived only as prose comments inside the closure
// plus a source-string pin (runshutdown_phase_timing_test.go). shutdownStep
// makes the sequence a value, so runshutdown_order_test.go can assert the
// ACTUAL call order with mock steps — a future subsystem (planner / Cron
// Dashboard / system session) inserted at the wrong index breaks a behavioral
// test, not just a grep.
package main

import (
	"log/slog"
	"time"
)

// shutdownStep is one ordered teardown phase. name is the slog `phase=`
// label; run performs the teardown (it may block, e.g. the HTTP drain
// barrier). A nil run is skipped (e.g. sysMgr absent) but its position in the
// sequence is still preserved so the contract order does not shift.
type shutdownStep struct {
	name string
	run  func()
}

// runShutdownSteps executes steps strictly in slice order, emitting a
// per-phase timing log line for each (R245-ARCH-38 / #893) and returning the
// names actually run (skipping nil-run steps) so tests can assert the
// observed call sequence. Steps with a nil run are logged-and-skipped: the
// sysession step is nil when no Manager was built, but its slot still anchors
// the contract that anything-with-a-run after it tears down later.
func runShutdownSteps(steps []shutdownStep) []string {
	ran := make([]string, 0, len(steps))
	for _, s := range steps {
		if s.run == nil {
			continue
		}
		t0 := time.Now()
		s.run()
		ran = append(ran, s.name)
		slog.Info("shutdown phase complete", "phase", s.name, "ms", time.Since(t0).Milliseconds())
	}
	return ran
}
