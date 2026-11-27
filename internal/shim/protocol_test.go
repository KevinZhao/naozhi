package shim

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseClientMsg_TableDriven(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		wantMsg ClientMsg
	}{
		{
			name:    "write message",
			input:   `{"type":"write","line":"{\"k\":\"v\"}"}`,
			wantMsg: ClientMsg{Type: "write", Line: `{"k":"v"}`},
		},
		{
			name:    "attach with token and seq",
			input:   `{"type":"attach","token":"dGVzdA==","last_seq":42}`,
			wantMsg: ClientMsg{Type: "attach", Token: "dGVzdA==", Seq: 42},
		},
		{
			name:    "extra unknown fields tolerated",
			input:   `{"type":"ping","unknown_field":true}`,
			wantMsg: ClientMsg{Type: "ping"},
		},
		{
			name:    "invalid json",
			input:   `not json`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseClientMsg([]byte(tc.input))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseClientMsg err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got.Type != tc.wantMsg.Type {
				t.Errorf("Type = %q, want %q", got.Type, tc.wantMsg.Type)
			}
			if got.Line != tc.wantMsg.Line {
				t.Errorf("Line = %q, want %q", got.Line, tc.wantMsg.Line)
			}
			if got.Token != tc.wantMsg.Token {
				t.Errorf("Token = %q, want %q", got.Token, tc.wantMsg.Token)
			}
			if got.Seq != tc.wantMsg.Seq {
				t.Errorf("Seq = %d, want %d", got.Seq, tc.wantMsg.Seq)
			}
		})
	}
}

func TestServerMsg_MarshalLine_TableDriven(t *testing.T) {
	cliAlive := true
	code := 42

	tests := []struct {
		name   string
		msg    ServerMsg
		checks []string // substrings that must appear in marshaled output
	}{
		{
			name:   "hello marshals all fields",
			msg:    ServerMsg{Type: "hello", ShimPID: 1, CLIPID: 2, CLIAlive: &cliAlive, SessionID: "s1", ProtocolVersion: ProtocolVersion},
			checks: []string{`"type":"hello"`, `"shim_pid":1`, `"cli_pid":2`, `"cli_alive":true`, `"session_id":"s1"`, `"protocol_version":1`},
		},
		{
			name:   "cli_exited with pointer code",
			msg:    ServerMsg{Type: "cli_exited", Code: &code},
			checks: []string{`"type":"cli_exited"`, `"code":42`},
		},
		{
			name:   "replay_done omits empty fields",
			msg:    ServerMsg{Type: "replay_done", Count: 3},
			checks: []string{`"type":"replay_done"`, `"count":3`},
		},
		{
			name:   "stdout with seq and line",
			msg:    ServerMsg{Type: "stdout", Seq: 100, Line: "hello world"},
			checks: []string{`"type":"stdout"`, `"seq":100`, `"line":"hello world"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := tc.msg.MarshalLine()
			if err != nil {
				t.Fatalf("MarshalLine err: %v", err)
			}
			// Must be valid JSON
			var raw map[string]interface{}
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatalf("output not valid JSON: %v", err)
			}
			s := string(data)
			for _, want := range tc.checks {
				if !strings.Contains(s, want) {
					t.Errorf("output %q missing %q", s, want)
				}
			}
		})
	}
}

// TestServerMsg_MarshalLine_NewlineTerminated locks down R65-PERF-L-2: the
// returned slice ends with exactly one '\n', so callers can enqueue it
// directly without a second append that would trigger a growslice copy on
// every CLI stdout line.
func TestServerMsg_MarshalLine_NewlineTerminated(t *testing.T) {
	msg := ServerMsg{Type: "stdout", Seq: 1, Line: "hello"}
	data, err := msg.MarshalLine()
	if err != nil {
		t.Fatalf("MarshalLine err: %v", err)
	}
	if n := len(data); n == 0 || data[n-1] != '\n' {
		t.Fatalf("expected trailing \\n, got %q", data)
	}
	// Exactly one newline — a second caller-side append would have doubled it.
	if nl := strings.Count(string(data), "\n"); nl != 1 {
		t.Fatalf("expected exactly one \\n, got %d in %q", nl, data)
	}
	// Round-trip still works (json.Unmarshal ignores trailing whitespace).
	var parsed ServerMsg
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if parsed.Type != "stdout" || parsed.Seq != 1 || parsed.Line != "hello" {
		t.Fatalf("roundtrip mismatch: %+v", parsed)
	}
}

// TestMarshalStdoutLine_WireEquivalent locks down R67-PERF-3: the fast
// stdout-frame encoder must produce byte-identical output to the generic
// MarshalLine() path so naozhi's decoder cannot tell the two apart.
// Covers the three classes of CLI stdout content the readStdout hot path
// actually produces: pure ASCII (most assistant_delta frames), embedded
// JSON-special bytes that demand escaping (tool_use payloads with quotes /
// backslashes), and multi-byte UTF-8 (CJK assistant text).
func TestMarshalStdoutLine_WireEquivalent(t *testing.T) {
	cases := []struct {
		name string
		seq  int64
		line string
	}{
		{"ascii", 1, `{"type":"assistant","text":"hello"}`},
		{"escaped", 42, "line with \"quote\" and \\ backslash and \nnewline"},
		{"utf8", 99, `{"text":"中文 🚀 ñ"}`},
		{"empty", 7, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fast, err := MarshalStdoutLine(tc.seq, []byte(tc.line))
			if err != nil {
				t.Fatalf("MarshalStdoutLine err: %v", err)
			}
			ref := ServerMsg{Type: "stdout", Seq: tc.seq, Line: tc.line}
			slow, err := ref.MarshalLine()
			if err != nil {
				t.Fatalf("MarshalLine err: %v", err)
			}
			if string(fast) != string(slow) {
				t.Fatalf("wire mismatch:\n fast=%q\n slow=%q", fast, slow)
			}
			var parsed ServerMsg
			if err := json.Unmarshal(fast, &parsed); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if parsed.Type != "stdout" || parsed.Seq != tc.seq || parsed.Line != tc.line {
				t.Fatalf("roundtrip mismatch: got %+v want seq=%d line=%q",
					parsed, tc.seq, tc.line)
			}
		})
	}
}

// TestMarshalReplayLine_WireEquivalent mirrors the stdout fast-path guard for
// the reconnect replay frame: MarshalReplayLine must be byte-identical to the
// generic MarshalLine() path so the replaying client decodes it the same as a
// string-copied frame would.
func TestMarshalReplayLine_WireEquivalent(t *testing.T) {
	cases := []struct {
		name string
		seq  int64
		line string
	}{
		{"ascii", 1, `{"type":"assistant","text":"hello"}`},
		{"escaped", 42, "line with \"quote\" and \\ backslash and \nnewline"},
		{"utf8", 99, `{"text":"中文 🚀 ñ"}`},
		{"empty", 7, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fast, err := MarshalReplayLine(tc.seq, []byte(tc.line))
			if err != nil {
				t.Fatalf("MarshalReplayLine err: %v", err)
			}
			ref := ServerMsg{Type: "replay", Seq: tc.seq, Line: tc.line}
			slow, err := ref.MarshalLine()
			if err != nil {
				t.Fatalf("MarshalLine err: %v", err)
			}
			if string(fast) != string(slow) {
				t.Fatalf("wire mismatch:\n fast=%q\n slow=%q", fast, slow)
			}
			var parsed ServerMsg
			if err := json.Unmarshal(fast, &parsed); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if parsed.Type != "replay" || parsed.Seq != tc.seq || parsed.Line != tc.line {
				t.Fatalf("roundtrip mismatch: got %+v want seq=%d line=%q",
					parsed, tc.seq, tc.line)
			}
		})
	}
}

// TestMarshalReplayLine_NoBufferAlias guards the same aliasing contract as the
// stdout counterpart: the returned frame must not retain a pointer into the
// caller's `line` buffer.
func TestMarshalReplayLine_NoBufferAlias(t *testing.T) {
	line := []byte(`{"k":"v"}`)
	data, err := MarshalReplayLine(5, line)
	if err != nil {
		t.Fatalf("MarshalReplayLine err: %v", err)
	}
	snapshot := append([]byte(nil), data...)
	for i := range line {
		line[i] = 'X'
	}
	if string(data) != string(snapshot) {
		t.Fatalf("wire frame aliased into caller buffer:\n before=%q\n after =%q",
			snapshot, data)
	}
}

// TestMarshalStdoutLine_NoBufferAlias guards the unsafe.String aliasing
// contract: MarshalStdoutLine must NOT return a slice that retains a
// pointer into the caller's `line` buffer. Otherwise mutating `line`
// after the call (which the shim's bufio.Scanner does on the very next
// Scan()) would corrupt the in-flight wire frame. We assert independence
// by overwriting `line` and confirming the returned bytes are unchanged.
func TestMarshalStdoutLine_NoBufferAlias(t *testing.T) {
	line := []byte(`{"k":"v"}`)
	data, err := MarshalStdoutLine(5, line)
	if err != nil {
		t.Fatalf("MarshalStdoutLine err: %v", err)
	}
	snapshot := append([]byte(nil), data...)
	// Simulate bufio.Scanner reusing its buffer: clobber the caller's
	// `line` storage. If json.Marshal had retained the alias, the
	// returned bytes would now contain garbage.
	for i := range line {
		line[i] = 'X'
	}
	if string(data) != string(snapshot) {
		t.Fatalf("wire frame aliased into caller buffer:\n before=%q\n after =%q",
			snapshot, data)
	}
}
