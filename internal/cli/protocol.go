package cli

import (
	"errors"
	"io"
)

// ErrInterruptUnsupported is returned by Protocol.WriteInterrupt for protocols
// that do not support mid-turn interrupt messages over stdin (e.g. ACP).
// Callers should fall back to SIGINT-based Interrupt() or to Collect mode.
var ErrInterruptUnsupported = errors.New("protocol does not support stdin interrupt")

// Protocol abstracts the communication protocol between naozhi and an AI CLI agent.
// Implementations handle protocol-specific message formats, initialization handshakes,
// and event parsing (e.g., Claude stream-json vs ACP JSON-RPC 2.0).
type Protocol interface {
	// Name returns the protocol identifier (e.g., "stream-json", "acp").
	Name() string

	// Clone returns a fresh Protocol instance for a new process.
	// Stateless protocols may return the receiver; stateful ones must return a new instance.
	Clone() Protocol

	// BuildArgs returns CLI arguments to launch the agent in this protocol mode.
	// For ACP, Model and ResumeID are handled via RPC in Init, not CLI flags.
	BuildArgs(opts SpawnOptions) []string

	// Init performs any handshake required after process spawn but before readLoop.
	// For stream-json: no-op. For ACP: sends initialize + session/new or session/load.
	// cwd is the workspace directory the agent should treat as its working directory
	// (ACP passes this in session/new params; stream-json inherits os.Chdir set by
	// the shim). Returns sessionID if established during init (empty if deferred to
	// first message).
	Init(rw *JSONRW, resumeID string, cwd string) (sessionID string, err error)

	// WriteMessage writes a user message (with optional images) to the agent's stdin.
	WriteMessage(w io.Writer, text string, images []ImageData) error

	// WriteUserMessageLocked is the passthrough-aware writer. The caller MUST
	// already hold the per-process stdin write lock (Process.shimWMu). For
	// protocols that do not honor uuid/priority (ACP), the extras are ignored
	// and behavior degrades to WriteMessage. Used so Process.Send can append
	// the sendSlot and write the NDJSON line under a single mutex — without
	// this, concurrent Send calls can interleave slot-append vs stdin-write
	// and break FIFO matching (see docs/rfc/passthrough-mode.md §5.2.2).
	WriteUserMessageLocked(w io.Writer, uuid, text string, images []ImageData, priority string) error

	// SupportsPriority reports whether this protocol forwards a top-level
	// "priority" field to the backing agent. Callers gate /urgent behavior on
	// this: false means "no `now` channel, fall back to interrupt+send".
	SupportsPriority() bool

	// SupportsReplay reports whether the backing agent echoes stdin user
	// messages as replay events (with round-tripped uuid). Required for
	// passthrough slot matching; when false the session falls back to Collect
	// mode regardless of queue.mode config.
	SupportsReplay() bool

	// WriteInterrupt writes an in-band interrupt request to the agent's stdin.
	// For stream-json, this emits the `control_request` message which the
	// Claude CLI honours by terminating the active turn (including killing
	// any in-flight tool invocation) and emitting a normal `result` event.
	// requestID is an opaque identifier echoed back in the control_response.
	// Protocols without stdin-level interrupt (e.g. ACP) return
	// ErrInterruptUnsupported.
	WriteInterrupt(w io.Writer, requestID string) error

	// ReadEvent parses a single NDJSON line from stdout into a unified Event.
	// Returns the event, whether this event completes the current turn, and any error.
	// Events that should be silently skipped return a zero Event with done=false, err=nil.
	ReadEvent(line string) (ev Event, done bool, err error)

	// HandleEvent allows the protocol to react to events (e.g., auto-grant permissions).
	// Returns true if the event was handled internally and should not be forwarded.
	HandleEvent(w io.Writer, ev Event) (handled bool)
}

// JSONRW provides line-oriented JSON read/write over stdin/stdout.
type JSONRW struct {
	W io.Writer
	R LineReader
}

// LineReader reads lines from a buffered source.
type LineReader interface {
	ReadLine() ([]byte, bool, error)
}

// WriteLine writes a JSON-encoded value followed by a newline.
func (rw *JSONRW) WriteLine(data []byte) error {
	_, err := rw.W.Write(append(data, '\n'))
	return err
}
