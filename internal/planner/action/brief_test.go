package action

import (
	"strings"
	"testing"
	"time"
)

// TestBrief: a dispatch's message wrapped in the relay envelope gives back
// the Planner's text, not the user's quoted words; a plain one is itself.
func TestBrief(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	wrapped := relayEnvelope(now, []Words{{Text: "就这么办", At: now}}, "把 X 修好\n\n细节：Y")
	if got := Brief(wrapped); got != "把 X 修好\n\n细节：Y" {
		t.Errorf("Brief(wrapped) = %q", got)
	}
	if got := Brief(relayEnvelope(now, []Words{{Text: "好", At: now}}, "")); got != "" {
		t.Errorf("Brief(envelope only) = %q", got)
	}
	if got := Brief(wrapped[:strings.Index(wrapped, "</"+envelopeTag)]); got != "" {
		t.Errorf("Brief(envelope cut short) = %q, want none of the user's words", got)
	}
	if got := Brief("  直接派的任务 \n"); got != "直接派的任务" {
		t.Errorf("Brief(plain) = %q", got)
	}
}
