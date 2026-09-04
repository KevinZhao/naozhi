package shim

import (
	"encoding/json"
	"unsafe"
)

// --- naozhi → shim ---

// ClientMsg is a message sent from naozhi to shim.
type ClientMsg struct {
	Type  string `json:"type"`               // attach, write, interrupt, close_stdin, kill, ping, shutdown, detach
	Line  string `json:"line,omitempty"`     // write: raw NDJSON line for CLI stdin
	Token string `json:"token,omitempty"`    // attach: auth token (base64)
	Seq   int64  `json:"last_seq,omitempty"` // attach: last received seq for replay
}

// --- shim → naozhi ---

// ServerMsg is a message sent from shim to naozhi.
type ServerMsg struct {
	Type  string `json:"type"`            // hello, replay, replay_done, stdout, stderr, cli_exited, pong, auth_failed, error
	Seq   int64  `json:"seq,omitempty"`   // stdout, replay: global sequence number
	Line  string `json:"line,omitempty"`  // stdout, replay, stderr: raw line content
	Count int    `json:"count,omitempty"` // replay_done: number of replayed lines

	// hello fields
	ShimPID         int    `json:"shim_pid,omitempty"`
	CLIPID          int    `json:"cli_pid,omitempty"`
	CLIAlive        *bool  `json:"cli_alive,omitempty"` // pointer: distinguishes false from absent
	SessionID       string `json:"session_id,omitempty"`
	BufferSeqStart  int64  `json:"buffer_seq_start,omitempty"`
	BufferSeqEnd    int64  `json:"buffer_seq_end,omitempty"`
	ProtocolVersion int    `json:"protocol_version,omitempty"`

	// cli_exited fields
	Code   *int   `json:"code,omitempty"` // pointer: distinguishes 0 from absent
	Signal string `json:"signal,omitempty"`

	// pong fields
	Buffered int `json:"buffered,omitempty"`

	// error / auth_failed
	Msg string `json:"msg,omitempty"`
}

// ProtocolVersion is the wire format version naozhi and shim handshake on.
// Bumped every time the JSON shape of ClientMsg / ServerMsg changes in a
// way that older peers cannot safely tolerate.
const ProtocolVersion = 1

// MinSupportedProtocolVersion is the oldest ProtocolVersion this binary
// accepts on a hello, giving a rolling deploy that bumps shim before naozhi
// (or vice versa) a well-defined window. Bumping it to N is the explicit
// "no longer accept ProtocolVersion < N" signal.
const MinSupportedProtocolVersion = 1

// boolPtr returns a pointer to b. Useful for ServerMsg fields that need explicit false.
func boolPtr(b bool) *bool { return &b }

// intPtr returns a pointer to i. Useful for ServerMsg fields that need explicit 0.
func intPtr(i int) *int { return &i }

// MarshalLine marshals a ServerMsg as a single NDJSON line including the
// trailing '\n', so callers can enqueue the slice without a second append.
func (m *ServerMsg) MarshalLine() ([]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// MarshalStdoutLine builds the NDJSON envelope for a "stdout" frame directly
// from the raw scanner bytes: unsafe.String aliases them into ServerMsg.Line
// so json.Marshal reads the scanner's buffer instead of a `string(line)` copy
// (hot path, 5–50 lines/s ×N sessions). Output is byte-for-byte identical to
// `(&ServerMsg{Type:"stdout",Seq:seq,Line:string(line)}).MarshalLine()`.
//
// SAFETY: the alias is confined to the synchronous Marshal (the encoder
// copies into its own buffer, so the result borrows nothing); callers must
// not advance the bufio.Scanner before the call returns.
func MarshalStdoutLine(seq int64, line []byte) ([]byte, error) {
	var lineStr string
	if len(line) > 0 {
		lineStr = unsafe.String(&line[0], len(line))
	}
	m := ServerMsg{Type: "stdout", Seq: seq, Line: lineStr}
	data, err := json.Marshal(&m)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// MarshalReplayLine is MarshalStdoutLine for "replay" frames, fed by the
// reconnect buffer-replay loop (avoids a per-line string copy on a 10k ring).
//
// SAFETY: `line` is aliased only across the synchronous json.Marshal; the
// caller writes each frame before advancing and LinesSince returns stable
// copies, so the bytes cannot mutate mid-marshal. Output is byte-for-byte
// identical to `(&ServerMsg{Type:"replay",Seq:seq,Line:string(line)}).MarshalLine()`.
func MarshalReplayLine(seq int64, line []byte) ([]byte, error) {
	var lineStr string
	if len(line) > 0 {
		lineStr = unsafe.String(&line[0], len(line))
	}
	m := ServerMsg{Type: "replay", Seq: seq, Line: lineStr}
	data, err := json.Marshal(&m)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// ParseClientMsg parses a single NDJSON line into a ClientMsg.
func ParseClientMsg(line []byte) (ClientMsg, error) {
	var msg ClientMsg
	err := json.Unmarshal(line, &msg)
	return msg, err
}

// ParseServerMsg parses a single NDJSON line into a ServerMsg. No in-tree
// consumer; kept as the symmetric public counterpart of ParseClientMsg for
// external decoders and round-trip tests.
func ParseServerMsg(line []byte) (ServerMsg, error) {
	var msg ServerMsg
	err := json.Unmarshal(line, &msg)
	return msg, err
}
