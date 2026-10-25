package wireup

import (
	"testing"

	"github.com/naozhi/naozhi/internal/discovery"
)

// TestThumbnailHookIsWired guards a hook with no build-time enforcement: with
// discovery.ThumbnailFn nil, image blocks in rehydrated JSONL history are
// silently dropped — no compile error, no warning, just missing pictures.
//
// Importing wireup wires nothing; main calls WireHistoryThumbnails, so the
// test calls it too.
func TestThumbnailHookIsWired(t *testing.T) {
	prev := discovery.ThumbnailFn
	t.Cleanup(func() { discovery.ThumbnailFn = prev })
	discovery.ThumbnailFn = nil

	WireHistoryThumbnails()

	if discovery.ThumbnailFn == nil {
		t.Fatal("WireHistoryThumbnails left discovery.ThumbnailFn nil — image blocks in JSONL history would be dropped with no other symptom")
	}
	// And it must actually produce a data URI, not just be non-nil: a stub that
	// returns "" would satisfy the nil check while dropping every image.
	if got := discovery.ThumbnailFn(pngFixture(), 64); got == "" {
		t.Error("ThumbnailFn returned empty for a valid PNG; the hook is wired to something that drops images")
	}
}

// pngFixture is a minimal valid 1x1 PNG.
func pngFixture() []byte {
	return []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89,
		0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A', 'T',
		0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
		0x0d, 0x0a, 0x2d, 0xb4,
		0x00, 0x00, 0x00, 0x00, 'I', 'E', 'N', 'D',
		0xae, 0x42, 0x60, 0x82,
	}
}
