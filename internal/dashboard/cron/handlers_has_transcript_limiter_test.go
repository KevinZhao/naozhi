package cron

import (
	"testing"
)

// TestHandlers_HasTranscriptLimiter pins R20260607-SEC-9: HasTranscriptLimiter
// must return false when transcriptLimiter is nil and true when it is wired.
// server.go relies on this method for the boot-time invariant panic; a wrong
// return value would either suppress a valid alert or fire a false one.
func TestHandlers_HasTranscriptLimiter_NilReturnsFalse(t *testing.T) {
	t.Parallel()
	h := &Handlers{} // transcriptLimiter is zero-value (nil)
	if h.HasTranscriptLimiter() {
		t.Fatal("HasTranscriptLimiter() = true for nil transcriptLimiter; want false")
	}
}

func TestHandlers_HasTranscriptLimiter_WiredReturnsTrue(t *testing.T) {
	t.Parallel()
	h := &Handlers{
		transcriptLimiter: newPerIPBurstNLimiter(1),
	}
	if !h.HasTranscriptLimiter() {
		t.Fatal("HasTranscriptLimiter() = false for wired transcriptLimiter; want true")
	}
}
