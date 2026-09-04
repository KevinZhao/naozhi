package cli

import (
	"errors"
	"io"
)

// ErrInterruptUnsupported is returned by Protocol.WriteInterrupt for protocols
// that do not support mid-turn interrupt messages over stdin (e.g. ACP).
// Callers should fall back to SIGINT-based Interrupt() or to Collect mode.
var ErrInterruptUnsupported = errors.New("protocol does not support stdin interrupt")

// ErrSetModelUnsupported is returned by Process.SetModel when the protocol does
// not implement ModelSetter (codex). Router.SetSessionTuning treats it as "record
// the override; it applies on the next spawn via --model", not a hard failure.
// docs/rfc/dashboard-model-effort-control.md §4.4.
var ErrSetModelUnsupported = errors.New("protocol does not support runtime model change")

// ModelSetter is the OPTIONAL Protocol facet for runtime model switching,
// surfaced via type assertion so the Protocol interface stays unchanged.
// Wire mapping (docs/rfc/dashboard-model-effort-control.md §1): ACP sends a
// session/set_model RPC whose response ReadEvent intercepts via its
// pending-control table and surfaces as a Type:"control_ack" Event (otherwise
// the generic IsResponse branch would close the active turn mid-flight);
// stream-json sends control_request {subtype:"set_model"} and parses the
// control_response into the same control_ack (rejection text in Result).
// requestID is echoed verbatim in the ack's RPCRequestID for both protocols,
// so Process.SetModel can register its ack channel BEFORE writing.
type ModelSetter interface {
	// WriteSetModel writes a runtime model-change request to stdin; the ack
	// arrives on the readLoop as a control_ack Event with RPCRequestID==requestID.
	WriteSetModel(w io.Writer, requestID, model string) error
}

// Protocol abstracts the communication protocol between naozhi and an AI CLI
// agent (Claude stream-json vs ACP / codex JSON-RPC 2.0): message formats,
// handshakes, and event parsing. ClaudeProtocol.ReadEvent is the reference
// semantics; ACP's "one wire frame → multiple Events" is the only divergence.
//
// Capability surface: SupportsPriority()/SupportsReplay() are the required
// minimum every implementation must satisfy; the opt-in Capabilities() Caps
// method (read via ProtocolCaps) overrides the default mapping. Both tracks are
// intentional — removing SupportsX breaks external implementations, while Caps
// grows fields without churn. New consumers should read ProtocolCaps(p).
type Protocol interface {
	// Name returns the protocol identifier (e.g., "stream-json", "acp").
	Name() string

	// Clone returns a fresh Protocol instance for a new process.
	// Stateless protocols may return the receiver; stateful ones must return a new instance.
	Clone() Protocol

	// BuildArgs returns CLI arguments to launch the agent in this protocol mode.
	// For ACP, Model and ResumeID are handled via RPC in Init, not CLI flags.
	BuildArgs(opts SpawnOptions) []string

	// Init performs any handshake required after spawn but before readLoop
	// (stream-json: no-op; ACP: initialize + session/new or session/load). cwd
	// is the agent's workspace (ACP passes it in session/new; stream-json
	// inherits the shim's os.Chdir). Returns sessionID if established here.
	Init(rw *JSONRW, resumeID string, cwd string) (sessionID string, err error)

	// WriteMessage writes a user message (with optional images) to the agent's stdin.
	WriteMessage(w io.Writer, text string, images []Attachment) error

	// WriteUserMessageLocked is the passthrough-aware writer; the caller MUST
	// hold Process.shimWMu so Process.Send can append the sendSlot and write
	// the NDJSON line under one mutex — otherwise concurrent Sends interleave
	// and break FIFO matching (docs/rfc/passthrough-mode.md §5.2.2).
	WriteUserMessageLocked(w io.Writer, uuid, text string, images []Attachment, priority string) error

	// SupportsPriority reports whether this protocol forwards a top-level
	// "priority" field; false means /urgent falls back to interrupt+send.
	SupportsPriority() bool

	// SupportsReplay reports whether the backing agent echoes stdin user
	// messages as replay events (with round-tripped uuid). Required for
	// passthrough slot matching; when false the session falls back to Collect
	// mode regardless of queue.mode config.
	SupportsReplay() bool

	// WriteInterrupt writes an in-band interrupt request to stdin. For
	// stream-json this is the `control_request` that ends the active turn
	// (killing in-flight tools) with a normal `result`; requestID is echoed in
	// the control_response. Protocols without one return ErrInterruptUnsupported.
	WriteInterrupt(w io.Writer, requestID string) error

	// ReadEvent parses a single NDJSON line from stdout into zero or more
	// unified Events. Lines to skip return a nil slice with done=false, err=nil.
	// done is advisory, NOT the turn-completion signal: every production caller
	// discards it. Turn-end is detected from the emitted events (Type=="result"
	// for claude/ACP, the codex turn-end frame), so an implementation MUST emit
	// a result/turn-end Event or the session stays stuck in state=running. A
	// slice lets ACP split the turn-closing JSON-RPC response into assistant
	// text + result; claude stream-json always returns one element.
	ReadEvent(line string) (events []Event, done bool, err error)

	// HandleEvent allows the protocol to react to events (e.g., auto-grant permissions).
	// Returns true if the event was handled internally and should not be forwarded.
	HandleEvent(w io.Writer, ev Event) (handled bool)
}

// eventReaderInto is the optional allocation-aware ReadEvent variant (#1676):
// the readLoop hands in a reused backing array via buf so the common
// single-event frame does not allocate a fresh 1-element []Event per line.
// Surfaced via type assertion (callers fall back to ReadEvent). The returned
// slice is backed by buf and only valid until the next call sharing that buf.
type eventReaderInto interface {
	// done mirrors ReadEvent's advisory flag; turn-end is a result/turn-end Event.
	ReadEventInto(line string, buf []Event) (events []Event, done bool, err error)
}

// ProtocolCore is the protocol-agnostic subset of Protocol that every backend
// implements without degrading to a noop or panic; the passthrough-flavoured
// methods live on ProtocolPassthroughExt. This is an additive facet split
// (#668): Protocol still embeds both facets, so nothing existing changes.
// Consumers that only ingest/parse events (transcript re-player, protocol
// probe) depend on ProtocolCore and stay compatible with backends that never
// grow the passthrough surface. The compile-time pins below keep the partition.
type ProtocolCore interface {
	// Name returns the protocol identifier (e.g., "stream-json", "acp").
	Name() string

	// Clone returns a fresh Protocol instance for a new process.
	Clone() Protocol

	// BuildArgs returns CLI arguments to launch the agent in this protocol mode.
	BuildArgs(opts SpawnOptions) []string

	// Init performs any handshake required after process spawn but before readLoop.
	Init(rw *JSONRW, resumeID string, cwd string) (sessionID string, err error)

	// WriteMessage writes a user message (with optional images) to the agent's stdin.
	WriteMessage(w io.Writer, text string, images []Attachment) error

	// ReadEvent parses a single NDJSON line into zero or more Events; done is
	// advisory (see Protocol.ReadEvent for the full contract).
	ReadEvent(line string) (events []Event, done bool, err error)

	// HandleEvent allows the protocol to react to events.
	HandleEvent(w io.Writer, ev Event) (handled bool)
}

// ProtocolPassthroughExt is the stream-json-specific surface of Protocol
// (#668); ACP / codex degrade these to noop / fallback / ErrInterruptUnsupported.
// The complement of ProtocolCore; a future refactor may shrink Protocol to
// ProtocolCore and reach this facet via type assertion.
type ProtocolPassthroughExt interface {
	// WriteUserMessageLocked is the passthrough-aware writer (caller holds the
	// per-process stdin write lock). ACP ignores the extras.
	WriteUserMessageLocked(w io.Writer, uuid, text string, images []Attachment, priority string) error

	// SupportsPriority reports whether this protocol forwards a top-level
	// "priority" field to the backing agent.
	SupportsPriority() bool

	// SupportsReplay reports whether the backing agent echoes stdin user
	// messages as replay events.
	SupportsReplay() bool

	// WriteInterrupt writes an in-band interrupt request to stdin.
	// Protocols without stdin-level interrupt return ErrInterruptUnsupported.
	WriteInterrupt(w io.Writer, requestID string) error
}

// Compile-time guarantees that ProtocolCore and ProtocolPassthroughExt are each
// a strict subset of Protocol (#668), so a method change on either side fails
// the build instead of letting the facets silently desync.
var (
	_ ProtocolCore           = (Protocol)(nil)
	_ ProtocolPassthroughExt = (Protocol)(nil)
)

// Caps aggregates Protocol capabilities so consumers feature-route via one
// accessor instead of individual SupportsX() methods. Only Replay is consumed
// on hot paths (passthrough.go, process_readloop.go); Priority, SoftInterrupt
// and StreamJSON are populated but unread — forward-compatibility anchors a
// future feature gate can read. protocol_caps_test.go pins the values.
type Caps struct {
	// Replay is true if the protocol echoes stdin user messages back with a
	// round-tripped uuid (Claude stream-json); required for passthrough matching.
	Replay bool
	// Priority is reserved. Set by ClaudeProtocol but not yet consumed —
	// today /urgent gates on Process.SupportsPriority() directly.
	Priority bool
	// SoftInterrupt is reserved. true if WriteInterrupt is a safe soft cancel
	// once a session is established (ACP session/cancel); pre-handshake calls
	// may still return ErrInterruptUnsupported (see ACPProtocol.WriteInterrupt).
	SoftInterrupt bool
	// StreamJSON is reserved. true if the wire format is stream-json (Claude);
	// anticipates a backend whose Name() does not encode the wire shape.
	StreamJSON bool
	// EffortTier is true if BuildArgs honours SpawnOptions.Effort (kiro's
	// `acp --effort`, Claude CLI `--effort` ≥ 2.1.226). The composition root
	// rejects `cli.backends[].effort` on a backend that would silently ignore
	// it. docs/rfc/kiro-effort-control.md §4.3
	EffortTier bool
}

// ProtocolCaps returns the capability set of any Protocol: an implementation's
// own Capabilities() method wins; otherwise Caps is derived from
// SupportsReplay / SupportsPriority / Name().
func ProtocolCaps(p Protocol) Caps {
	if cp, ok := p.(interface{ Capabilities() Caps }); ok {
		return cp.Capabilities()
	}
	return Caps{
		Replay:     p.SupportsReplay(),
		Priority:   p.SupportsPriority(),
		StreamJSON: p.Name() != "acp",
		// Fallback derivation for protocols without Capabilities(). A backend
		// that grows a tier flag or a soft interrupt should implement
		// Capabilities() rather than widen this default.
		EffortTier: p.Name() == "acp",
	}
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
