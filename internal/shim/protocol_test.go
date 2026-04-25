package shim

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseClientMsg_TableDriven(t *testing.T) {
	trueVal := true

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
			name:    "interrupt",
			input:   `{"type":"interrupt"}`,
			wantMsg: ClientMsg{Type: "interrupt"},
		},
		{
			name:    "close_stdin",
			input:   `{"type":"close_stdin"}`,
			wantMsg: ClientMsg{Type: "close_stdin"},
		},
		{
			name:    "kill",
			input:   `{"type":"kill"}`,
			wantMsg: ClientMsg{Type: "kill"},
		},
		{
			name:    "ping",
			input:   `{"type":"ping"}`,
			wantMsg: ClientMsg{Type: "ping"},
		},
		{
			name:    "shutdown",
			input:   `{"type":"shutdown"}`,
			wantMsg: ClientMsg{Type: "shutdown"},
		},
		{
			name:    "detach",
			input:   `{"type":"detach"}`,
			wantMsg: ClientMsg{Type: "detach"},
		},
		{
			name:    "empty type",
			input:   `{"type":""}`,
			wantMsg: ClientMsg{Type: ""},
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
		{
			name:    "empty object",
			input:   `{}`,
			wantMsg: ClientMsg{},
		},
	}

	_ = trueVal
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

func TestParseServerMsg_TableDriven(t *testing.T) {
	aliveTrue := true
	aliveFalse := false
	code0 := 0
	code1 := 1

	tests := []struct {
		name    string
		input   string
		wantErr bool
		check   func(t *testing.T, msg ServerMsg)
	}{
		{
			name:  "hello message full",
			input: `{"type":"hello","shim_pid":100,"cli_pid":200,"cli_alive":true,"session_id":"sess1","buffer_seq_start":1,"buffer_seq_end":10,"protocol_version":1}`,
			check: func(t *testing.T, msg ServerMsg) {
				if msg.Type != "hello" {
					t.Errorf("Type = %q, want hello", msg.Type)
				}
				if msg.ShimPID != 100 {
					t.Errorf("ShimPID = %d, want 100", msg.ShimPID)
				}
				if msg.CLIPID != 200 {
					t.Errorf("CLIPID = %d, want 200", msg.CLIPID)
				}
				if msg.CLIAlive == nil || *msg.CLIAlive != true {
					t.Errorf("CLIAlive = %v, want &true", msg.CLIAlive)
				}
				if msg.SessionID != "sess1" {
					t.Errorf("SessionID = %q, want sess1", msg.SessionID)
				}
				if msg.BufferSeqStart != 1 || msg.BufferSeqEnd != 10 {
					t.Errorf("BufferSeqStart/End = %d/%d, want 1/10", msg.BufferSeqStart, msg.BufferSeqEnd)
				}
				if msg.ProtocolVersion != 1 {
					t.Errorf("ProtocolVersion = %d, want 1", msg.ProtocolVersion)
				}
			},
		},
		{
			name:  "stdout message",
			input: `{"type":"stdout","seq":42,"line":"some output line"}`,
			check: func(t *testing.T, msg ServerMsg) {
				if msg.Type != "stdout" || msg.Seq != 42 || msg.Line != "some output line" {
					t.Errorf("unexpected stdout msg: %+v", msg)
				}
			},
		},
		{
			name:  "replay message",
			input: `{"type":"replay","seq":7,"line":"buffered line"}`,
			check: func(t *testing.T, msg ServerMsg) {
				if msg.Type != "replay" || msg.Seq != 7 || msg.Line != "buffered line" {
					t.Errorf("unexpected replay msg: %+v", msg)
				}
			},
		},
		{
			name:  "replay_done with count",
			input: `{"type":"replay_done","count":5}`,
			check: func(t *testing.T, msg ServerMsg) {
				if msg.Type != "replay_done" || msg.Count != 5 {
					t.Errorf("unexpected replay_done msg: %+v", msg)
				}
			},
		},
		{
			name:  "cli_exited with code 0",
			input: `{"type":"cli_exited","code":0}`,
			check: func(t *testing.T, msg ServerMsg) {
				if msg.Type != "cli_exited" {
					t.Errorf("Type = %q, want cli_exited", msg.Type)
				}
				if msg.Code == nil || *msg.Code != 0 {
					t.Errorf("Code = %v, want &0", msg.Code)
				}
			},
		},
		{
			name:  "cli_exited with non-zero code",
			input: `{"type":"cli_exited","code":1,"signal":"SIGTERM"}`,
			check: func(t *testing.T, msg ServerMsg) {
				if msg.Code == nil || *msg.Code != 1 {
					t.Errorf("Code = %v, want &1", msg.Code)
				}
				if msg.Signal != "SIGTERM" {
					t.Errorf("Signal = %q, want SIGTERM", msg.Signal)
				}
			},
		},
		{
			name:  "pong with buffered count",
			input: `{"type":"pong","cli_alive":false,"buffered":99}`,
			check: func(t *testing.T, msg ServerMsg) {
				if msg.Type != "pong" {
					t.Errorf("Type = %q, want pong", msg.Type)
				}
				if msg.CLIAlive == nil || *msg.CLIAlive != false {
					t.Errorf("CLIAlive = %v, want &false", msg.CLIAlive)
				}
				if msg.Buffered != 99 {
					t.Errorf("Buffered = %d, want 99", msg.Buffered)
				}
			},
		},
		{
			name:  "auth_failed with msg",
			input: `{"type":"auth_failed","msg":"invalid token"}`,
			check: func(t *testing.T, msg ServerMsg) {
				if msg.Type != "auth_failed" || msg.Msg != "invalid token" {
					t.Errorf("unexpected auth_failed: %+v", msg)
				}
			},
		},
		{
			name:  "error message",
			input: `{"type":"error","msg":"something went wrong"}`,
			check: func(t *testing.T, msg ServerMsg) {
				if msg.Type != "error" || msg.Msg != "something went wrong" {
					t.Errorf("unexpected error msg: %+v", msg)
				}
			},
		},
		{
			name:  "stderr message",
			input: `{"type":"stderr","line":"stderr output"}`,
			check: func(t *testing.T, msg ServerMsg) {
				if msg.Type != "stderr" || msg.Line != "stderr output" {
					t.Errorf("unexpected stderr msg: %+v", msg)
				}
			},
		},
		{
			name:    "invalid json",
			input:   `{bad json}`,
			wantErr: true,
		},
	}

	_ = aliveTrue
	_ = aliveFalse
	_ = code0
	_ = code1

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseServerMsg([]byte(tc.input))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseServerMsg err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if tc.check != nil {
				tc.check(t, got)
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

func TestBoolPtr(t *testing.T) {
	tr := boolPtr(true)
	fa := boolPtr(false)

	if tr == nil || *tr != true {
		t.Errorf("boolPtr(true) = %v", tr)
	}
	if fa == nil || *fa != false {
		t.Errorf("boolPtr(false) = %v", fa)
	}
	// Must return independent pointers
	if tr == fa {
		t.Error("boolPtr returned same pointer for different values")
	}
}

func TestIntPtr(t *testing.T) {
	p0 := intPtr(0)
	p1 := intPtr(1)

	if p0 == nil || *p0 != 0 {
		t.Errorf("intPtr(0) = %v", p0)
	}
	if p1 == nil || *p1 != 1 {
		t.Errorf("intPtr(1) = %v", p1)
	}
	if p0 == p1 {
		t.Error("intPtr returned same pointer for different values")
	}
}

func TestProtocolVersion_Constant(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Errorf("ProtocolVersion = %d, want 1", ProtocolVersion)
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
	// Round-trip still works (ParseServerMsg ignores trailing whitespace).
	parsed, err := ParseServerMsg(data)
	if err != nil {
		t.Fatalf("ParseServerMsg: %v", err)
	}
	if parsed.Type != "stdout" || parsed.Seq != 1 || parsed.Line != "hello" {
		t.Fatalf("roundtrip mismatch: %+v", parsed)
	}
}
