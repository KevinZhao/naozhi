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
	// DefaultTaskTotalTimeout is the floor of a task: session's total
	// budget: a Planner-dispatched task (a deep research, a benchmark of
	// hours) outruns the interactive default, and a turn killed at 2h is
	// read by custody as a retryable failure and told to continue.
	DefaultTaskTotalTimeout = 6 * time.Hour
)
