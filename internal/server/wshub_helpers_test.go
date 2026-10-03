package server

import (
	"encoding/json"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// TestValidateProjectName locks in R68-SEC-M3: project `name` query param
// must be gated at the HTTP boundary so oversized or control-character
// inputs cannot reach slog attrs.
func TestValidateProjectName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"empty rejected", "", false},
		{"simple ASCII", "myproj", true},
		{"Chinese passes", "项目甲", true},
		{"128 bytes exact", string(make([]byte, project.MaxProjectNameBytes)), false}, // NUL chars reject
		{"128 printable exact", repeatByte('a', project.MaxProjectNameBytes), true},
		{"129 bytes rejected", repeatByte('a', project.MaxProjectNameBytes+1), false},
		{"NUL rejected", "foo\x00bar", false},
		{"LF rejected", "foo\nbar", false},
		{"CR rejected", "foo\rbar", false},
		{"tab rejected", "foo\tbar", false},
		{"ESC rejected", "foo\x1bbar", false},
		{"DEL rejected", "foo\x7fbar", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := project.ValidateProjectName(c.in)
			if c.ok && err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			if !c.ok && err == nil {
				t.Errorf("expected error for %q", c.in)
			}
		})
	}
}

func repeatByte(b byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

// TestWSPreMarshalledFrames locks the byte-for-byte contract between the
// wsproto pre-encoded frames the hub sends raw and the JSON output the
// equivalent SendJSON(node.ServerMsg{...}) call would produce. Field
// reordering or omitempty changes in either struct family would silently
// break older clients that rely on a specific JSON shape, so any drift
// must surface here. R229-PERF-4.
func TestWSPreMarshalledFrames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  string // SendRaw call sites convert via []byte(...)
		msg  node.ServerMsg
	}{
		{"not authenticated", wsproto.RawErrNotAuth, node.ServerMsg{Type: "error", Error: "not authenticated"}},
		{"rate limited", wsproto.RawErrRateLimited, node.ServerMsg{Type: "error", Error: "rate limited"}},
		{"auth ok", wsproto.RawAuthOK, node.ServerMsg{Type: "auth_ok"}},
		{"pong", wsproto.RawPong, node.ServerMsg{Type: "pong"}},
		{"auth fail invalid", wsproto.RawAuthFailInvalid, node.ServerMsg{Type: "auth_fail", Error: "invalid token"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if tc.got != string(want) {
				t.Fatalf("pre-marshalled frame drift\n  got=%s\n want=%s", tc.got, want)
			}
		})
	}
}
