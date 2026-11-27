package cli

import (
	"encoding/base64"
	"testing"
)

// TestEncodeImageBase64_RoundTrip pins R247-PERF-17 correctness: the pooled
// encode path must produce the exact same string as the standard
// base64.StdEncoding.EncodeToString. The pool optimisation only changes
// where the encode buffer comes from; output bytes must be identical or the
// JSON `Data` field would silently corrupt the on-wire image payload.
func TestEncodeImageBase64_RoundTrip(t *testing.T) {
	t.Parallel()
	cases := [][]byte{
		nil,
		{},
		{0x00},
		{0xFF, 0xD8, 0xFF, 0xE0}, // jpeg magic prefix
		[]byte("hello world"),
		make([]byte, 1),
		make([]byte, 1024),
		make([]byte, 16*1024),
		make([]byte, 16*1024+1), // forces buffer growth past pool seed
		make([]byte, 64*1024+1), // forces drop on Put (oversize)
	}
	for i, img := range cases {
		want := base64.StdEncoding.EncodeToString(img)
		got := encodeImageBase64(img)
		if got != want {
			t.Errorf("case %d (len=%d): got %q…, want %q…", i, len(img),
				truncateHead(got), truncateHead(want))
		}
	}
}

func truncateHead(s string) string {
	if len(s) > 32 {
		return s[:32]
	}
	return s
}
