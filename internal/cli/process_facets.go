package cli

import "context"

// Facet interfaces over cli.Process (#902) so consumers depend on the narrow
// seam they use; the compile-time pins below catch method drift at build time.

// ProcessLifecycle is the start/stop/liveness control surface of a Process
// (cli-side analogue of session.ProcessLifecycle, #430).
type ProcessLifecycle interface {
	// Alive reports whether the process has not yet exited.
	Alive() bool
	// IsRunning reports whether the process is actively running a turn.
	IsRunning() bool
	// Close performs a graceful shutdown, waiting for the shim teardown.
	Close()
	// Kill force-terminates the process (SIGUSR2 shim fast-path).
	Kill()
	// Detach releases the shim connection without killing the CLI.
	Detach()
	// PID returns the underlying CLI process id (0 if unknown).
	PID() int
	// DeathReason returns a human-readable reason once the process exits.
	DeathReason() string
}

// ProcessTurnIO is the per-turn input surface of a Process: send a user
// message (Collect or Passthrough mode) and interrupt an in-flight turn.
type ProcessTurnIO interface {
	// Send writes a user message and collects the resulting turn.
	Send(ctx context.Context, text string, images []Attachment, onEvent EventCallback) (*SendResult, error)
	// SendPassthrough writes a user message in passthrough mode with an optional priority.
	SendPassthrough(ctx context.Context, text string, images []Attachment, onEvent EventCallback, priority string) (*SendResult, error)
	// Interrupt requests cancellation of the active turn (SIGINT path).
	Interrupt()
	// InterruptViaControl requests cancellation via stream-json control_request;
	// returns ErrInterruptUnsupported for protocols without it.
	InterruptViaControl() error
}

// ProcessIntrospect is the read-only metadata surface of a Process used by
// dashboard Snapshot paths.
type ProcessIntrospect interface {
	// State returns the current lifecycle state.
	State() ProcessState
	// SessionID returns the established session id (empty if deferred).
	SessionID() string
	// ProtocolName returns the backing protocol identifier.
	ProtocolName() string
	// Model returns the model identifier reported by the backend.
	Model() string
}

// Compile-time guarantees that *Process satisfies each facet (#902).
var (
	_ ProcessLifecycle  = (*Process)(nil)
	_ ProcessTurnIO     = (*Process)(nil)
	_ ProcessIntrospect = (*Process)(nil)
)
