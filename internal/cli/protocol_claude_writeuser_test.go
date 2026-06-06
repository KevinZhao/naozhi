package cli

import (
	"bytes"
	"encoding/json"
	"testing"
)

// legacyMarshalUserMessage reproduces the exact bytes the pre-#1826
// WriteUserMessageLocked produced: json.Marshal(msg) + a manual trailing '\n'.
// The pooled-encoder implementation must stay byte-for-byte identical to this.
func legacyMarshalUserMessage(t *testing.T, uuid, text string, images []ImageData, priority string) []byte {
	t.Helper()
	msg := NewUserMessageWithMeta(text, images, uuid, priority)
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("legacy json.Marshal: %v", err)
	}
	return append(data, '\n')
}

func TestWriteUserMessageLocked_ByteIdenticalToMarshal(t *testing.T) {
	t.Parallel()

	pngBytes := []byte("fake-png-data\x00\x01\xff") // includes a non-UTF8 byte
	cases := []struct {
		name     string
		uuid     string
		text     string
		images   []ImageData
		priority string
	}{
		{name: "text only", text: "hello world"},
		{name: "empty text"},
		// HTML-significant chars: json.Marshal escapes <, >, & to < etc.
		// The pooled encoder must keep HTML escaping ON to match.
		{name: "html chars", text: "if a < b && c > d use <tag>"},
		{name: "with uuid", uuid: "11111111-2222-3333-4444-555555555555", text: "with id"},
		{name: "with priority", text: "urgent", priority: "now"},
		{name: "uuid and priority", uuid: "abc-123", text: "both", priority: "next"},
		{name: "unicode", text: "café   line   sep 你好"},
		{name: "control chars", text: "tab\there\nnewline\rcr"},
		{
			name:   "single image plus text",
			text:   "describe <this>",
			images: []ImageData{{Data: pngBytes, MimeType: "image/png"}},
		},
		{
			name: "multi image",
			text: "compare",
			images: []ImageData{
				{Data: []byte("img-a"), MimeType: "image/png"},
				{Data: []byte("img-b"), MimeType: "image/jpeg"},
			},
		},
		{
			name:   "image only no text",
			images: []ImageData{{Data: pngBytes, MimeType: "image/png"}},
		},
		{
			name: "image with uuid and priority and html",
			uuid: "deadbeef",
			text: "look at <img> & report",
			images: []ImageData{
				{Data: []byte("zzz"), MimeType: "image/gif"},
			},
			priority: "later",
		},
		{
			name: "file ref attachment",
			text: "read it",
			images: []ImageData{
				{Kind: KindFileRef, WorkspacePath: "docs/spec.pdf", OrigName: "spec.pdf", MimeType: "application/pdf", Size: 2048},
			},
		},
	}

	p := &ClaudeProtocol{}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := legacyMarshalUserMessage(t, tc.uuid, tc.text, tc.images, tc.priority)

			var buf bytes.Buffer
			if err := p.WriteUserMessageLocked(&buf, tc.uuid, tc.text, tc.images, tc.priority); err != nil {
				t.Fatalf("WriteUserMessageLocked: %v", err)
			}
			if got := buf.Bytes(); !bytes.Equal(got, want) {
				t.Errorf("output mismatch\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

// TestWriteUserMessageLocked_TrailingNewline asserts exactly one trailing '\n'
// (matching the json.Encoder framing + the old manual append) and no extra
// bytes after it.
func TestWriteUserMessageLocked_TrailingNewline(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	var buf bytes.Buffer
	if err := p.WriteUserMessageLocked(&buf, "", "hi", nil, ""); err != nil {
		t.Fatal(err)
	}
	out := buf.Bytes()
	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Fatalf("expected trailing newline, got %q", out)
	}
	if bytes.Count(out, []byte{'\n'}) != 1 {
		t.Fatalf("expected exactly one newline, got %q", out)
	}
}

// TestWriteUserMessageLocked_WriteMessageDelegation confirms the WriteMessage
// shim (empty uuid/priority) produces the same bytes as the explicit call.
func TestWriteUserMessageLocked_WriteMessageDelegation(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	imgs := []ImageData{{Data: []byte("x"), MimeType: "image/png"}}

	var a, b bytes.Buffer
	if err := p.WriteMessage(&a, "hello & <world>", imgs); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteUserMessageLocked(&b, "", "hello & <world>", imgs, ""); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("WriteMessage vs WriteUserMessageLocked differ:\n a=%q\n b=%q", a.Bytes(), b.Bytes())
	}
}

// TestWriteUserMessageLocked_Reuse runs many sends through the same protocol to
// exercise the pool get/put cycle and confirm a recycled encoder/buffer does
// not leak prior content into a later message.
func TestWriteUserMessageLocked_Reuse(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	for i := 0; i < 100; i++ {
		text := "message-with-<html>-and-&-amp"
		want := legacyMarshalUserMessage(t, "", text, nil, "")
		var buf bytes.Buffer
		if err := p.WriteUserMessageLocked(&buf, "", text, nil, ""); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("iter %d mismatch\n got: %q\nwant: %q", i, buf.Bytes(), want)
		}
	}
}

func BenchmarkWriteUserMessageLocked(b *testing.B) {
	p := &ClaudeProtocol{}
	var sink discardWriter
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.WriteUserMessageLocked(sink, "uuid-1234", "explain this <code> & that", nil, "next"); err != nil {
			b.Fatal(err)
		}
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
