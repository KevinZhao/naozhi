package agentcore

import (
	"encoding/json"
	"errors"
	"testing"
)

func envCLI(t *testing.T, line string) *Envelope {
	t.Helper()
	return &Envelope{Kind: KindCLI, Line: json.RawMessage(line)}
}

// TestClassifier_ThreeStates pins the §6.1 classification table — the input
// to the §6.2 double-run containment rules. Wrong classification here makes
// an unsafe replay look safe, so every row is a security-relevant contract.
func TestClassifier_ThreeStates(t *testing.T) {
	tests := []struct {
		name      string
		envs      []*Envelope
		streamErr error
		want      TerminalState
	}{
		{
			name: "result ok + exit = success",
			envs: []*Envelope{
				envCLI(t, `{"type":"system","subtype":"init"}`),
				envCLI(t, `{"type":"result","subtype":"success","is_error":false}`),
				{Kind: KindExit, Code: 0},
			},
			want: Success,
		},
		{
			name: "result is_error=true = failed-clean (CLI self-reported, V4 fc probe)",
			envs: []*Envelope{
				envCLI(t, `{"type":"result","subtype":"success","is_error":true}`),
				{Kind: KindExit, Code: 1},
			},
			want: FailedClean,
		},
		{
			name: "no result but exit frame + clean EOF = failed-clean (handler attested death)",
			envs: []*Envelope{
				envCLI(t, `{"type":"system","subtype":"init"}`),
				{Kind: KindExit, Code: 137},
			},
			want: FailedClean,
		},
		{
			name: "stream error = failed-transport regardless of progress",
			envs: []*Envelope{
				envCLI(t, `{"type":"system","subtype":"init"}`),
			},
			streamErr: errors.New("connection reset"),
			want:      FailedTransport,
		},
		{
			name: "result seen but stream error after = failed-transport (conservatism)",
			envs: []*Envelope{
				envCLI(t, `{"type":"result","is_error":false}`),
			},
			streamErr: errors.New("reset before exit frame"),
			want:      FailedTransport,
		},
		{
			name: "clean EOF, no result, no exit = failed-transport (V8 idle-burn shape)",
			envs: []*Envelope{
				envCLI(t, `{"type":"system","subtype":"init"}`),
				envCLI(t, `{"type":"assistant"}`),
			},
			want: FailedTransport,
		},
		{
			name: "empty stream clean EOF = failed-transport",
			envs: nil,
			want: FailedTransport,
		},
		{
			name: "keepalive and boot frames do not affect classification",
			envs: []*Envelope{
				{Kind: KindKeepalive},
				{Kind: KindBoot, Msg: "materialized"},
				envCLI(t, `{"type":"result","is_error":false}`),
				{Kind: KindKeepalive},
				{Kind: KindExit, Code: 0},
			},
			want: Success,
		},
		{
			name: "non-result cli lines never classify (tool_use with result-ish text)",
			envs: []*Envelope{
				envCLI(t, `{"type":"assistant","message":{"content":[{"type":"text","text":"result is_error false"}]}}`),
				{Kind: KindExit, Code: 0},
			},
			want: FailedClean,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c classifier
			for _, e := range tt.envs {
				c.observe(e)
			}
			if got := c.terminal(tt.streamErr); got != tt.want {
				t.Fatalf("terminal() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsResultLine_MalformedJSON(t *testing.T) {
	if isRes, _ := isResultLine(json.RawMessage(`{not json`)); isRes {
		t.Fatal("malformed line must not classify as result")
	}
	if isRes, _ := isResultLine(nil); isRes {
		t.Fatal("nil line must not classify as result")
	}
}
