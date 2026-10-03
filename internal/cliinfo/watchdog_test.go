package cliinfo

import (
	"testing"
	"time"
)

// TestWatchdogDefaults_OutlastKnownSilences pins the floor the no-output
// default must clear: the Claude CLI's own 600s API timeout (it retries a
// stalled request itself and says so with system/api_retry) and its 30s
// tool_progress heartbeat. The total budget must leave room for several such
// silences in one turn.
func TestWatchdogDefaults_OutlastKnownSilences(t *testing.T) {
	const cliAPITimeout = 600 * time.Second
	if DefaultNoOutputTimeout <= cliAPITimeout {
		t.Errorf("DefaultNoOutputTimeout = %v, want > the CLI's %v API timeout", DefaultNoOutputTimeout, cliAPITimeout)
	}
	if DefaultTotalTimeout < 4*DefaultNoOutputTimeout {
		t.Errorf("DefaultTotalTimeout = %v, want >= 4 x DefaultNoOutputTimeout (%v)", DefaultTotalTimeout, DefaultNoOutputTimeout)
	}
}
