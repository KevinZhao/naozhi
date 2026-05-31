package cli

import "context"

// Facet interfaces over the cli.Process god-struct (R245-ARCH-42, #902).
//
// Process today is a ~60-field struct whose ~40 exported methods span
// lifecycle control, turn IO, and dashboard introspection. The issue's
// long-term proposal is to facet-split the *fields* into
// processCore/processStream/processTurn substructs — a large, invasive
// change that touches every method receiver. This file lands the
// ADDITIVE first step: facet *interfaces* over the existing method set,
// so consumers (session/server) can depend on the narrow seam they
// actually use instead of the whole concrete *Process. No field is
// moved, no method signature changes, no caller is touched.
//
// The compile-time `var _ Facet = (*Process)(nil)` pins below guarantee
// *Process keeps satisfying each facet, so a method rename/removal fails
// this package's build rather than silently breaking a downstream
// narrowing. Mirrors the session-package facet split in #430 and the
// ProtocolCore/ProtocolPassthroughExt split in #668 (protocol.go).

// ProcessLifecycle is the start/stop/liveness control surface of a
// Process. It is the cli-side analogue of session.ProcessLifecycle
// (#430): a consumer that only needs to observe or tear down a process
// (e.g. a janitor sweeping dead sessions) can depend on this instead of
// the full *Process.
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
// A dispatcher that only feeds turns and cancels them can depend on this
// rather than the whole struct.
type ProcessTurnIO interface {
	// Send writes a user message and collects the resulting turn.
	Send(ctx context.Context, text string, images []ImageData, onEvent EventCallback) (*SendResult, error)
	// SendPassthrough writes a user message in passthrough mode with an
	// optional top-level priority channel.
	SendPassthrough(ctx context.Context, text string, images []ImageData, onEvent EventCallback, priority string) (*SendResult, error)
	// Interrupt requests cancellation of the active turn (SIGINT path).
	Interrupt()
	// InterruptViaControl requests cancellation over the stdin control
	// channel (stream-json control_request); returns ErrInterruptUnsupported
	// for protocols without it.
	InterruptViaControl() error
}

// ProcessIntrospect is the read-only metadata surface of a Process used
// by dashboard Snapshot paths: identity, state and protocol info. None
// of these mutate the process.
type ProcessIntrospect interface {
	// GetState returns the current lifecycle state.
	GetState() ProcessState
	// GetSessionID returns the established session id (empty if deferred).
	GetSessionID() string
	// ProtocolName returns the backing protocol identifier.
	ProtocolName() string
	// Model returns the model identifier reported by the backend.
	Model() string
}

// Compile-time guarantees that *Process satisfies each facet
// (R245-ARCH-42, #902). A signature change or method removal on Process
// fails the package build here instead of letting a downstream narrowing
// silently break. These pins are additive — they impose no new
// requirement Process did not already meet.
var (
	_ ProcessLifecycle  = (*Process)(nil)
	_ ProcessTurnIO     = (*Process)(nil)
	_ ProcessIntrospect = (*Process)(nil)
)
