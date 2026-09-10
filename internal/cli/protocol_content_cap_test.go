package cli

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestClaudeReadEvent_ContentByteCap pins R229-SEC-10: ReadEvent must drop
// events whose clievent.AssistantMessage.Content total byte size exceeds
// clievent.MaxAssistantMessageContentBytes so a tampered or buggy CLI cannot
// amplify a single event into multi-MiB downstream work across every
// ring.EventLog ring / dashboard fan-out / JSONL persist consumer.
func TestClaudeReadEvent_ContentByteCap(t *testing.T) {
	// Build a single oversized text block (cap + 1 byte) embedded in a
	// well-formed assistant event. JSON encoding adds a few bytes but the
	// content cap looks at the parsed Text length, not the wire size.
	bigText := strings.Repeat("A", clievent.MaxAssistantMessageContentBytes+1)
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + bigText + `"}]}}`

	p := &ClaudeProtocol{}
	events, _, err := p.ReadEvent(line)
	if err == nil {
		t.Fatalf("expected error for oversized content, got nil; events=%d", len(events))
	}
	if events != nil {
		t.Errorf("expected nil events on cap exceeded, got %d", len(events))
	}
}

// TestClaudeReadEvent_AtCapAccepted pins that an event exactly at the cap
// is still accepted — the boundary is "exceeds", not "equals".
func TestClaudeReadEvent_AtCapAccepted(t *testing.T) {
	// Cap minus envelope overhead so the parsed Text matches the cap exactly.
	bigText := strings.Repeat("B", clievent.MaxAssistantMessageContentBytes)
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + bigText + `"}]}}`

	p := &ClaudeProtocol{}
	events, _, err := p.ReadEvent(line)
	if err != nil {
		t.Fatalf("unexpected error at exact cap: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("expected 1 event at cap, got %d", len(events))
	}
}
