package dispatch

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestPrepareInbound_ValidMessage pins R20260531A-ARCH-3 (#1527): for an
// ordinary text message prepareInbound returns ok=true with the resolved key,
// agent, and cleaned text so the dispatch-strategy tail can proceed.
func TestPrepareInbound_ValidMessage(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)

	p, ok := d.prepareInbound(context.Background(), incomingMsg("hello world"))
	if !ok {
		t.Fatal("prepareInbound should accept an ordinary text message")
	}
	if p.agentID != "general" {
		t.Errorf("agentID = %q, want general", p.agentID)
	}
	if p.cleanText != "hello world" {
		t.Errorf("cleanText = %q, want %q", p.cleanText, "hello world")
	}
	if p.key == "" {
		t.Error("key should be resolved")
	}
	if p.lg == nil {
		t.Error("logger should be set")
	}
}

// TestPrepareInbound_DedupAndGate pins the early-return paths: a duplicate
// event ID and an un-mentioned group message must both yield ok=false.
func TestPrepareInbound_DedupAndGate(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	ctx := context.Background()

	msg := platform.IncomingMessage{
		Platform: "fake", EventID: "evt-dup",
		UserID: "u", ChatID: "c", ChatType: "direct", Text: "hi",
	}
	if _, ok := d.prepareInbound(ctx, msg); !ok {
		t.Fatal("first delivery should be accepted")
	}
	if _, ok := d.prepareInbound(ctx, msg); ok {
		t.Error("duplicate event ID must be dropped (ok=false)")
	}

	group := platform.IncomingMessage{
		Platform: "fake", EventID: "evt-group",
		UserID: "u", ChatID: "g", ChatType: "group", Text: "hi", MentionMe: false,
	}
	if _, ok := d.prepareInbound(ctx, group); ok {
		t.Error("un-mentioned group message must be gated (ok=false)")
	}
}

// TestPrepareInbound_EmptyTextDropped verifies the empty-body guard returns
// ok=false (with no agent prefix → no reply).
func TestPrepareInbound_EmptyTextDropped(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	if _, ok := d.prepareInbound(context.Background(), incomingMsg("   ")); ok {
		t.Error("whitespace-only message must be dropped (ok=false)")
	}
}
