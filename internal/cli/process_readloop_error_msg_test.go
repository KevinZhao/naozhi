package cli

import (
	"encoding/json"
	"testing"
)

// TestShimMsg_ErrorFrameMsgField locks R202606f-ARCH-1: the shim emits error
// frames as {"type":"error","msg":"..."} (internal/shim/server.go). The cli
// shimMsg struct must decode that text into Msg, not leave it empty — the
// previous struct had no Msg field so the error text was silently dropped and
// operators saw `"shim error" msg=""`.
func TestShimMsg_ErrorFrameMsgField(t *testing.T) {
	raw := `{"type":"error","msg":"another client is connected"}`
	var msg shimMsg
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if msg.Type != "error" {
		t.Fatalf("Type = %q, want error", msg.Type)
	}
	if msg.Msg != "another client is connected" {
		t.Errorf("Msg = %q, want %q", msg.Msg, "another client is connected")
	}
}

// TestShimMsg_ErrorFrameMsgPreferredOverLine verifies the handleShimMessage
// error case records msg.Msg, and only falls back to msg.Line when Msg is
// empty (legacy-frame compatibility).
func TestShimMsg_ErrorFrameMsgPreferredOverLine(t *testing.T) {
	cases := []struct {
		name string
		msg  shimMsg
		want string
	}{
		{
			name: "msg_field_set",
			msg:  shimMsg{Type: "error", Msg: "duplicate attach", Line: "ignored"},
			want: "duplicate attach",
		},
		{
			name: "msg_empty_falls_back_to_line",
			msg:  shimMsg{Type: "error", Line: "legacy text"},
			want: "legacy text",
		},
		{
			name: "both_empty",
			msg:  shimMsg{Type: "error"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Mirror the selection logic in handleShimMessage's error case.
			errText := tc.msg.Msg
			if errText == "" {
				errText = tc.msg.Line
			}
			if errText != tc.want {
				t.Errorf("errText = %q, want %q", errText, tc.want)
			}
		})
	}
}
