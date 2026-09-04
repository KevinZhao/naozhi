// Graceful-shutdown teardown sequence. The sysMgr → scheduler → http-drain →
// router order is a hard contract (each earlier phase still calls into the
// later ones), expressed as a value so runshutdown_order_test.go can assert
// the actual call order (#1487).
package main

import (
	"log/slog"
	"time"
)

// shutdownStep is one ordered teardown phase; name is the slog `phase=` label.
// A nil run is skipped but keeps its slot so the contract order does not shift.
type shutdownStep struct {
	name string
	run  func()
}

// runShutdownSteps executes steps strictly in slice order with a per-phase
// timing log line and returns the names actually run (nil-run steps skipped).
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
