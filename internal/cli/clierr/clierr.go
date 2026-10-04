// Package clierr holds the sentinel errors that cross internal/cli's boundary.
// G1 (#2545 direction 1).
//
// These sixteen values are matched with errors.Is by internal/usermsg,
// internal/dispatch, internal/session, internal/server and internal/dashboard to
// decide what to tell the user. Before this package they lived in
// internal/cli/process.go and protocol.go, so a package that only needed to
// recognise "the CLI timed out" had to import the whole process manager —
// internal/usermsg imported internal/cli for NOTHING but these values, and got
// the subprocess spawner, the shim link and the event ring transitively.
//
// Definitions moved verbatim, including their doc comments: the message strings
// are load-bearing. Several reach the user through internal/usermsg, and one
// (ErrOrphanedSlot) exists only to make a bug identifiable in logs.
//
// Protocol-internal sentinels stayed in internal/cli — ErrACPRPC, ErrACPTimeout,
// ErrCodexRPC, ErrCodexTimeout are never matched outside the package, so moving
// them would widen this package's meaning from "errors callers of the CLI layer
// react to" without giving any caller anything.
package clierr

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// ErrMessageTooLarge is returned when a user message (after JSON encoding) would
// exceed the shim's per-line limit; callers should shrink the payload first.
var ErrMessageTooLarge = errors.New("message too large for stream-json line")

// Sentinel errors for watchdog timeouts.
var (
	ErrNoOutputTimeout = errors.New("no output timeout")
	ErrTotalTimeout    = errors.New("total timeout")
)

// NoOutputTimeoutError is the error a no-output watchdog kill returns. It
// matches ErrNoOutputTimeout under errors.Is; errors.As adds what was silent:
// Tool names the oldest tool still running at the kill (empty when none was,
// i.e. the model itself went quiet) and ToolElapsed how long it had run.
type NoOutputTimeoutError struct {
	Timeout     time.Duration
	Tool        string
	ToolElapsed time.Duration
}

func (e *NoOutputTimeoutError) Error() string {
	if e.Tool == "" {
		return fmt.Sprintf("%s (%s)", ErrNoOutputTimeout, e.Timeout)
	}
	return fmt.Sprintf("%s (%s, tool %q running %s)", ErrNoOutputTimeout, e.Timeout, e.Tool, e.ToolElapsed.Round(time.Second))
}

// Unwrap exposes ErrNoOutputTimeout, so every errors.Is classifier keeps working.
func (e *NoOutputTimeoutError) Unwrap() error { return ErrNoOutputTimeout }

// ErrProcessExited is returned by Send when the CLI subprocess exits before
// producing a result; callers react by spawning a new process next turn.
var ErrProcessExited = errors.New("process exited during send")

// ExitClass is what a CLI's stderr says made it exit with a non-zero code
// before producing any output; ExitUnknown when it says nothing recognisable.
type ExitClass int

const (
	ExitUnknown ExitClass = iota
	ExitResumeNotFound
	ExitAuth
	ExitMCPConfig
	ExitMissingRuntime
)

// exitClassWires are the ExitClass names the dashboard reads, by value.
var exitClassWires = [...]string{
	ExitUnknown:        "unknown",
	ExitResumeNotFound: "resume_not_found",
	ExitAuth:           "auth",
	ExitMCPConfig:      "mcp_config",
	ExitMissingRuntime: "missing_runtime",
}

// Wire is c's name in the dashboard API; "unknown" for a value out of range.
func (c ExitClass) Wire() string {
	if c < 0 || int(c) >= len(exitClassWires) {
		return exitClassWires[ExitUnknown]
	}
	return exitClassWires[c]
}

// AllExitClassWires lists every Wire name, in ExitClass order.
func AllExitClassWires() []string { return slices.Clone(exitClassWires[:]) }

// ProcessExitedError is ErrProcessExited for a CLI that exited with a
// non-zero code. It matches ErrProcessExited under errors.Is; errors.As adds
// the exit code and the class of its stderr. Error() leaves the stderr text
// out: send errors can reach IM replies.
type ProcessExitedError struct {
	Code  int64
	Class ExitClass
}

func (e *ProcessExitedError) Error() string {
	return fmt.Sprintf("%s (code %d)", ErrProcessExited, e.Code)
}

// Unwrap exposes ErrProcessExited, so every errors.Is classifier keeps working.
func (e *ProcessExitedError) Unwrap() error { return ErrProcessExited }

// ErrResumeRejected is returned by Spawn when the backend refused the session
// it was asked to resume during the Init handshake (an RPC error, or the CLI
// exiting); the session is unusable, so callers spawn fresh instead.
var ErrResumeRejected = errors.New("backend rejected the resumed session")

// ErrSpawnInit marks a Spawn error from the Init handshake (an RPC error, a
// timeout, or the CLI exiting before it answered): the CLI was started but
// could not be brought up, unlike a shim that failed to start.
var ErrSpawnInit = errors.New("CLI init handshake failed")

// ErrProcessBusy is returned by Send when the legacy (non-passthrough) state
// machine is already StateRunning; dispatch maps it to "正在处理中".
var ErrProcessBusy = errors.New("process busy")

// Passthrough-mode sentinels (separate block for targeted errors.Is switches).
var (
	// ErrSessionReset fires when a user slash-command (/new, /clear) or a forced
	// wrapper reset cancels all pending sends; not surfaced to IM (user-triggered).
	ErrSessionReset = errors.New("session reset")

	// ErrReconnectedUnknown fires when naozhi re-attaches to a shim+CLI that
	// survived a restart with messages in flight: naozhi cannot tell which were
	// consumed, so every pending slot gets it (dispatcher: 状态未知，请查看历史或重发).
	ErrReconnectedUnknown = errors.New("reconnected: processing state unknown")

	// ErrTooManyPending fires when Send is called with maxPendingSlots already
	// pending; the message is rejected up front (dispatcher: sendAckBusy).
	ErrTooManyPending = errors.New("too many pending messages")

	// ErrOrphanedSlot is SendPassthrough's backstop: the turn the CLI owes the
	// queue ran 30s past totalTimeout and neither the watchdog nor readLoop
	// delivered an outcome. Fires only on bugs.
	ErrOrphanedSlot = errors.New("slot orphaned: no result or error received")
)

// ErrAbortedByUrgent fires when a priority:"now" message makes the CLI drop the
// in-flight turn: older pending slots not yet replayed get this error — their
// text never reached the model, so the user must decide whether to resend.
var ErrAbortedByUrgent = errors.New("aborted by priority:now preemption")

// ErrNoActiveTurn is returned by InterruptViaControl when no turn is running;
// nothing was interrupted, so logs must not claim "aborted active turn".
var ErrNoActiveTurn = errors.New("no active turn to interrupt")

// ErrSetModelRejected is returned when the CLI refuses a runtime model change.
var ErrSetModelRejected = errors.New("set_model rejected by CLI")

// ErrInterruptUnsupported is returned by a Protocol that cannot interrupt via
// stdin; the caller falls back to killing the process.
var ErrInterruptUnsupported = errors.New("protocol does not support stdin interrupt")

// ErrSetModelUnsupported is returned by a Protocol that cannot change the model
// at runtime; the caller respawns with the new model instead.
var ErrSetModelUnsupported = errors.New("protocol does not support runtime model change")
