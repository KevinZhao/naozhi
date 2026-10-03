package cliinfo

import "time"

// Turn watchdog defaults, used when session.watchdog leaves a budget unset.
// The no-output budget only has to outlast silences no heartbeat covers (a
// long thinking block, a large Write input, the CLI's own 600s API retry):
// the Claude CLI emits a tool_progress frame every 30s while a tool runs. The
// total budget bounds one turn; Router.Cleanup's stuck detector fires at 2×.
const (
	DefaultNoOutputTimeout = 15 * time.Minute
	DefaultTotalTimeout    = 2 * time.Hour
)
