package shim

import (
	"strings"
	"testing"
)

// TestHandleClientCommand_OversizeWriteDisconnects pins R67-SEC-5 / #697:
// the "write" verb must signal disconnect when the line exceeds
// maxWriteLineBytes so a hostile client cannot wedge the single-client
// semaphore slot. handleClientCommand was extracted from the runCommandLoop
// switch in #697; this test verifies the disconnect contract still fires
// post-extraction.
func TestHandleClientCommand_OversizeWriteDisconnects(t *testing.T) {
	s := makeShimServerForTest(t)
	// Build a write payload one byte over the cap. The handler must
	// return disconnect=true without ever touching s.cli (nil here, so
	// any attempt to deref would panic the test cleanly).
	msg := ClientMsg{
		Type: "write",
		Line: strings.Repeat("x", int(maxWriteLineBytesValue())+1),
	}
	if got := s.handleClientCommand(msg); !got {
		t.Fatalf("oversize write should request disconnect, got false")
	}
}

// TestHandleClientCommand_DetachDisconnects pins the detach verb's
// "disconnect but keep shim running" contract: handleClientCommand
// returns disconnect=true while s.done remains open so the parent
// runCommandLoop unwinds the per-client teardown without firing
// initiateShutdown. R237-CR-3 (#697).
func TestHandleClientCommand_DetachDisconnects(t *testing.T) {
	s := makeShimServerForTest(t)
	if got := s.handleClientCommand(ClientMsg{Type: "detach"}); !got {
		t.Fatalf("detach should request disconnect, got false")
	}
	// done must still be open — detach is not shutdown.
	select {
	case <-s.done:
		t.Fatalf("detach must not close s.done")
	default:
	}
}

// TestHandleClientCommand_UnknownTypeContinues pins the default-arm
// behaviour of the verb switch: unknown / future message types must
// keep the loop running rather than dropping the client. Mirrors the
// pre-extraction "no return on unknown" branch. R237-CR-3 (#697).
func TestHandleClientCommand_UnknownTypeContinues(t *testing.T) {
	s := makeShimServerForTest(t)
	if got := s.handleClientCommand(ClientMsg{Type: "future_unknown"}); got {
		t.Fatalf("unknown verb must NOT request disconnect, got true")
	}
}
