package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestAppendJSONStringBytes_MatchesEncoder pins R245-PERF-1: the
// hand-rolled JSON-string escape used by shimSendLine must produce
// byte-identical output to encoding/json with SetEscapeHTML(false).
// Any drift would corrupt the shim wire format and surface as parse
// errors on the shim peer's reader.
func TestAppendJSONStringBytes_MatchesEncoder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []byte
	}{
		{"plain ascii", []byte("hello world")},
		{"empty", []byte("")},
		{"with quote", []byte(`he said "hi"`)},
		{"with backslash", []byte(`a\b\c`)},
		{"with newline", []byte("line1\nline2")},
		{"with tab", []byte("a\tb")},
		{"with carriage return", []byte("a\rb")},
		{"with backspace", []byte("a\bb")},
		{"with formfeed", []byte("a\fb")},
		{"control char 0x01", []byte{0x01, 'x'}},
		{"all c0 mix", []byte{0x00, 0x01, 0x1F, 'a'}},
		{"html chars unescaped", []byte("<div>&amp;</div>")},
		{"cjk", []byte("中文测试")},
		{"emoji", []byte("👋hi")},
		{"line separator U+2028", []byte("a b")},
		{"paragraph separator U+2029", []byte("a b")},
		{"invalid utf8 lone byte", []byte{'a', 0xFF, 'b'}},
		{"big payload", bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 200)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendJSONStringBytes(nil, tc.in)

			// Reference: encoding/json on the same string with
			// SetEscapeHTML(false). Strip the trailing '\n' the encoder
			// appends.
			var refBuf bytes.Buffer
			enc := json.NewEncoder(&refBuf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(string(tc.in)); err != nil {
				t.Fatalf("ref encoder: %v", err)
			}
			ref := bytes.TrimRight(refBuf.Bytes(), "\n")

			if !bytes.Equal(got, ref) {
				t.Errorf("drift\n got %q\n ref %q", got, ref)
			}
		})
	}
}

// TestShimSendLine_FrameByteEqual pins the assembled shim wire frame
// produced by the shimSendLine path against what the prior
// shimSend(shimClientMsg{Type: "write", Line: string(data)}) would emit.
// We replicate the framing without an actual shim socket.
func TestShimSendLine_FrameByteEqual(t *testing.T) {
	t.Parallel()
	cases := [][]byte{
		[]byte("plain ascii"),
		[]byte(`with "quotes" and \\`),
		[]byte("中文 emoji 👋"),
		[]byte("line\twith\tcontrol"),
		[]byte("<html>&amp;</html>"),
		bytes.Repeat([]byte("X"), 4096),
	}
	for i, line := range cases {
		t.Run("case_"+strings.TrimSpace(safeSnippet(line)), func(t *testing.T) {
			// Build the new path's frame the same way shimSendLine does.
			tmp := append([]byte(nil), shimWriteLineFramePrefix...)
			tmp = appendJSONStringBytes(tmp, line)
			tmp = append(tmp, shimWriteLineFrameSuffix...)

			// Build the old path's frame via encodeShimMsg.
			se, err := encodeShimMsg(shimClientMsg{Type: "write", Line: string(line)})
			if err != nil {
				t.Fatalf("ref encodeShimMsg: %v", err)
			}
			defer returnShimSendEnc(se)
			old := se.buf.Bytes()

			if !bytes.Equal(tmp, old) {
				t.Errorf("case %d frame drift\n new %q\n old %q", i, tmp, old)
			}
		})
	}
}

func safeSnippet(b []byte) string {
	const max = 20
	if len(b) > max {
		b = b[:max]
	}
	return string(b)
}
