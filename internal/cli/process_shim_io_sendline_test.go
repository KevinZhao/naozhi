package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestAppendJSONStringBytes_MatchesEncoder pins R245-PERF-1: for valid
// UTF-8 input the hand-rolled JSON-string escape used by shimSendLine must
// produce byte-identical output to encoding/json with SetEscapeHTML(false).
// Any drift would corrupt the shim wire format and surface as parse
// errors on the shim peer's reader. Invalid UTF-8 is pinned separately by
// TestAppendJSONStringBytes_InvalidUTF8Golden.
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
		{"literal U+FFFD", []byte("a\ufffdb")},
		{"line separator U+2028", []byte("a b")},
		{"paragraph separator U+2029", []byte("a b")},
		{"big payload", bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 200)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendJSONStringBytes(nil, tc.in)
			ref := stdlibJSONString(t, tc.in)

			if !bytes.Equal(got, ref) {
				t.Errorf("drift\n got %q\n ref %q", got, ref)
			}
		})
	}
}

// stdlibJSONString is the encoding/json reference with SetEscapeHTML(false),
// minus the trailing newline Encode appends.
func stdlibJSONString(t testing.TB, in []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(string(in)); err != nil {
		t.Fatalf("ref encoder: %v", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// decodeJSONString unmarshals a JSON string literal, failing the test on error.
func decodeJSONString(t testing.TB, label string, b []byte) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s: unmarshal %q: %v", label, b, err)
	}
	return s
}

// TestAppendJSONStringBytes_InvalidUTF8Golden pins the encoder's own contract
// for invalid UTF-8: one \ufffd escape per invalid byte and 7-bit-clean output.
// encoding/json changed its bytes here in go1.27 (raw EF BF BD), so the stdlib
// comparison is semantic: both encodings must decode to string([]rune(in)).
func TestAppendJSONStringBytes_InvalidUTF8Golden(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"lone byte", []byte{'a', 0xFF, 'b'}, `"a\ufffdb"`},
		{"truncated 3-byte sequence", []byte("\xe4\xb8"), `"\ufffd\ufffd"`},
		{"overlong encoding", []byte("\xc0\xaf"), `"\ufffd\ufffd"`},
		{"encoded surrogate", []byte("\xed\xa0\x80"), `"\ufffd\ufffd\ufffd"`},
		{"invalid between cjk", []byte("中\xff文"), "\"中\\ufffd文\""},
		{"invalid after escape", []byte("\"\x80\n"), `"\"\ufffd\n"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendJSONStringBytes(nil, tc.in)
			if string(got) != tc.want {
				t.Errorf("golden drift\n got %q\nwant %q", got, tc.want)
			}
			if !utf8.Valid(got) {
				t.Errorf("output is not valid UTF-8: %q", got)
			}
			want := string([]rune(string(tc.in)))
			if dec := decodeJSONString(t, "ours", got); dec != want {
				t.Errorf("ours decodes to %q, want %q", dec, want)
			}
			if dec := decodeJSONString(t, "stdlib", stdlibJSONString(t, tc.in)); dec != want {
				t.Errorf("stdlib decodes to %q, want %q", dec, want)
			}
		})
	}
}

// TestAppendJSONStringBytes_InvalidBytesStay7BitClean pins that invalid UTF-8
// never leaks a raw high byte: ASCII plus every invalid lead/continuation byte
// must encode to pure ASCII.
func TestAppendJSONStringBytes_InvalidBytesStay7BitClean(t *testing.T) {
	t.Parallel()
	in := []byte("x")
	for b := 0x80; b <= 0xFF; b++ {
		in = append(in, byte(b), 'x')
	}
	got := appendJSONStringBytes(nil, in)
	for i, b := range got {
		if b >= utf8.RuneSelf {
			t.Fatalf("raw byte %#x at offset %d in %q", b, i, got)
		}
	}
	if n := bytes.Count(got, []byte(`\ufffd`)); n != 0x80 {
		t.Errorf("got %d \\ufffd escapes, want %d", n, 0x80)
	}
}

// FuzzAppendJSONStringBytes checks, for arbitrary input, that the output is
// valid UTF-8 JSON decoding to string([]rune(in)) — the same string stdlib's
// encoding decodes to — and byte-identical to stdlib when in is valid UTF-8.
func FuzzAppendJSONStringBytes(f *testing.F) {
	for _, seed := range []string{
		"", "hello", `q"b\s`, "a\tb\nc\x00\x1f\x7f", "<&>", "中文👋",
		"a\u2028b\u2029c", "a\xffb", "\xe4\xb8", "\xc0\xaf", "\xed\xa0\x80",
		"a\ufffdb",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		got := appendJSONStringBytes(nil, in)
		if !utf8.Valid(got) || !json.Valid(got) {
			t.Fatalf("not valid UTF-8 JSON: %q", got)
		}
		ref := stdlibJSONString(t, in)
		want := string([]rune(string(in)))
		if dec := decodeJSONString(t, "ours", got); dec != want {
			t.Fatalf("ours decodes to %q, want %q", dec, want)
		}
		if dec := decodeJSONString(t, "stdlib", ref); dec != want {
			t.Fatalf("stdlib decodes to %q, want %q", dec, want)
		}
		if utf8.Valid(in) && !bytes.Equal(got, ref) {
			t.Fatalf("drift on valid UTF-8\n got %q\n ref %q", got, ref)
		}
	})
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
