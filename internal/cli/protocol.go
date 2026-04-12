package cli

import (
	"io"
)

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
	// Returns sessionID if established during init (empty if deferred to first message).
	Init(rw *JSONRW, resumeID string) (sessionID string, err error)

	// WriteMessage writes a user message (with optional images) to the agent's stdin.
	WriteMessage(w io.Writer, text string, images []ImageData) error

	// ReadEvent parses a single NDJSON line from stdout into a unified Event.
	// Returns the event, whether this event completes the current turn, and any error.
	// Events that should be silently skipped return a zero Event with done=false, err=nil.
	ReadEvent(line []byte) (ev Event, done bool, err error)

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
