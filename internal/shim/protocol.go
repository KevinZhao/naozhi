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
// will accept on a hello. R230B-ARCH-22 / RNEW-ARCH-403: keeps the
// version negotiation window distinct from the current protocol so a
// rolling deploy that bumps shim before naozhi (or vice versa) has a
// well-defined transition window. While both peers ship the same
// constant, this is just defence-in-depth — a mismatched binary refuses
// the connection cleanly instead of mis-parsing fields it doesn't
// understand.
//
// Today MinSupportedProtocolVersion == ProtocolVersion == 1; bumping to
// 2 should advance both, and bumping MinSupportedProtocolVersion to N
// is the explicit "we no longer accept ProtocolVersion < N" signal.
const MinSupportedProtocolVersion = 1

// boolPtr returns a pointer to b. Useful for ServerMsg fields that need explicit false.
func boolPtr(b bool) *bool { return &b }

// intPtr returns a pointer to i. Useful for ServerMsg fields that need explicit 0.
func intPtr(i int) *int { return &i }

// MarshalLine marshals a ServerMsg as a single NDJSON line, including a trailing
// '\n'. Callers can enqueue the returned slice directly without a second append
// that would otherwise trigger a growslice copy on every CLI stdout line.
// R65-PERF-L-2.
func (m *ServerMsg) MarshalLine() ([]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// MarshalStdoutLine builds the NDJSON envelope for a "stdout" frame directly
// from the raw scanner bytes, skipping the intermediate `string(line)` copy
// that the generic MarshalLine path would otherwise force.
//
// R67-PERF-3: the shim's readStdout hot path runs at every CLI stdout line
// (5–50/s during active turns, ×N concurrent sessions). The previous code
// did `string(line)` to populate ServerMsg.Line, then json.Marshal walked
// that string a second time to produce the JSON-escaped output — i.e. two
// passes over the byte content per line. By aliasing the scanner bytes into
// a string header via unsafe.String we hand json.Marshal the same backing
// memory the bufio.Scanner already owns, so there is zero extra alloc for
// the line content itself. Output is byte-for-byte identical to
// `(&ServerMsg{Type:"stdout",Seq:seq,Line:string(line)}).MarshalLine()`,
// round-trip tested via ParseServerMsg.
//
// SAFETY: the returned []byte must be fully consumed (or copied into the
// caller's enqueue buffer) BEFORE the caller advances bufio.Scanner. The
// shim's enqueueWrite path satisfies this — it copies the slice into a
// per-client channel before the next Scan().
func MarshalStdoutLine(seq int64, line []byte) ([]byte, error) {
	// unsafe.String produces a string header backed by `line`'s storage;
	// json.Marshal only reads the string during this call and the result
	// holds none of the borrowed bytes (the JSON encoder copies escaped
	// runes into its output buffer). Aliasing is therefore confined to
	// the synchronous Marshal call below.
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

// ParseClientMsg parses a single NDJSON line into a ClientMsg.
func ParseClientMsg(line []byte) (ClientMsg, error) {
	var msg ClientMsg
	err := json.Unmarshal(line, &msg)
	return msg, err
}

// ParseServerMsg parses a single NDJSON line into a ServerMsg.
//
// No in-tree consumer: naozhi is the *server* side of this protocol — it
// writes ServerMsg and reads ClientMsg (see ParseClientMsg). This helper
// is kept as the symmetric counterpart of ParseClientMsg and is part of
// the protocol's public contract surface, so a future client / log-tail
// inspector can decode server output without copy-pasting the unmarshal
// boilerplate. Round-trip-tested in protocol_test.go.
func ParseServerMsg(line []byte) (ServerMsg, error) {
	var msg ServerMsg
	err := json.Unmarshal(line, &msg)
	return msg, err
}
