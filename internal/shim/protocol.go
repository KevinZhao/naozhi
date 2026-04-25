package shim

import (
	"encoding/json"
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

const ProtocolVersion = 1

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

// ParseClientMsg parses a single NDJSON line into a ClientMsg.
func ParseClientMsg(line []byte) (ClientMsg, error) {
	var msg ClientMsg
	err := json.Unmarshal(line, &msg)
	return msg, err
}

// ParseServerMsg parses a single NDJSON line into a ServerMsg.
func ParseServerMsg(line []byte) (ServerMsg, error) {
	var msg ServerMsg
	err := json.Unmarshal(line, &msg)
	return msg, err
}
