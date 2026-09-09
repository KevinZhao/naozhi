// Package clierr holds the sentinel errors that cross internal/cli's boundary.
// G1 (#2545 direction 1).
//
// These fourteen values are matched with errors.Is by internal/usermsg,
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

import "errors"

// ErrMessageTooLarge is returned when a user message (after JSON encoding) would
// exceed the shim's per-line limit; callers should shrink the payload first.
var ErrMessageTooLarge = errors.New("message too large for stream-json line")

// Sentinel errors for watchdog timeouts.
var (
	ErrNoOutputTimeout = errors.New("no output timeout")
	ErrTotalTimeout    = errors.New("total timeout")
)

// ErrProcessExited is returned by Send when the CLI subprocess exits before
// producing a result; callers react by spawning a new process next turn.
var ErrProcessExited = errors.New("process exited during send")

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

	// ErrOrphanedSlot is a defensive fallback: Send's totalTimeout+30s tripwire in
	// case watchdog and readLoop both miss delivering a result. Fires only on bugs.
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
