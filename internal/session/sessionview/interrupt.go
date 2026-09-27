package sessionview

import "fmt"

// InterruptOutcome describes what happened on an InterruptViaControl call, so
// log messages can reflect the actual state instead of a bare bool.
type InterruptOutcome int

const (
	// InterruptSent — a control_request reached the CLI; the active turn
	// will produce a final result shortly and the next Send() will drain it.
	InterruptSent InterruptOutcome = iota
	// InterruptNoSession — session does not exist or has no live process.
	InterruptNoSession
	// InterruptNoTurn — session is alive but idle; nothing was interrupted.
	InterruptNoTurn
	// InterruptUnsupported — protocol does not support stdin-level interrupt
	// (e.g. ACP). Callers may fall back to Interrupt() for SIGINT semantics.
	InterruptUnsupported
	// InterruptError — transport failure; the process-level settle flags have
	// been rolled back. Callers should log this as an error.
	InterruptError
)

// String renders an InterruptOutcome as a stable lowercase tag for slog attrs.
func (o InterruptOutcome) String() string {
	switch o {
	case InterruptSent:
		return "sent"
	case InterruptNoSession:
		return "no_session"
	case InterruptNoTurn:
		return "no_turn"
	case InterruptUnsupported:
		return "unsupported"
	case InterruptError:
		return "error"
	default:
		return fmt.Sprintf("unknown(%d)", int(o))
	}
}
