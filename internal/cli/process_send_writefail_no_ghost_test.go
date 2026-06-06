package cli

import (
	"context"
	"errors"
	"io"
	"testing"
)

// writeMessageFailingProtocol is a minimal Protocol whose WriteMessage
// always fails. Used by TestProcess_Send_WriteMessageFail_NoGhostUserEntry
// to drive the failure branch in Process.Send so we can assert the
// EventLog was NOT mutated.
type writeMessageFailingProtocol struct{}

func (writeMessageFailingProtocol) Name() string                      { return "fake-fail" }
func (p writeMessageFailingProtocol) Clone() Protocol                 { return p }
func (writeMessageFailingProtocol) BuildArgs(_ SpawnOptions) []string { return nil }
func (writeMessageFailingProtocol) Init(_ *JSONRW, _, _ string) (string, error) {
	return "", nil
}

var errFakeWriteMessage = errors.New("fake protocol: write rejected")

func (writeMessageFailingProtocol) WriteMessage(_ io.Writer, _ string, _ []ImageData) error {
	return errFakeWriteMessage
}

func (writeMessageFailingProtocol) WriteUserMessageLocked(_ io.Writer, _, _ string, _ []ImageData, _ string) error {
	return errFakeWriteMessage
}

func (writeMessageFailingProtocol) SupportsPriority() bool { return false }
func (writeMessageFailingProtocol) SupportsReplay() bool   { return false }

func (writeMessageFailingProtocol) WriteInterrupt(_ io.Writer, _ string) error {
	return ErrInterruptUnsupported
}

func (writeMessageFailingProtocol) ReadEvent(_ string) ([]Event, bool, error) {
	return nil, false, nil
}

func (writeMessageFailingProtocol) HandleEvent(_ io.Writer, _ Event) bool {
	return false
}

// TestProcess_Send_WriteMessageFail_NoGhostUserEntry locks in
// R20260527122801-GO-002 (#?): when Protocol.WriteMessage fails, the
// EventLog must not contain a user entry for the rejected message.
//
// Pre-fix the user entry was appended BEFORE WriteMessage, so a rejected
// stdin write left a permanent ghost user bubble in history (Send
// returned an error to the caller, but the dashboard transcript and any
// PersistSink already saw an entry that the CLI never received).
//
// Mirrors the passthrough.go fix at line ~132 — both outbound paths must
// commit the user bubble only after the CLI accepts the bytes.
func TestProcess_Send_WriteMessageFail_NoGhostUserEntry(t *testing.T) {
	proto := writeMessageFailingProtocol{}
	p := &Process{
		protocol:    proto,
		caps:        ProtocolCaps(proto),
		state:       StateReady,
		eventCh:     make(chan Event, 8),
		done:        make(chan struct{}),
		eventLog:    NewEventLog(0),
		stdinWriter: nil, // unused: WriteMessage returns before touching the writer
	}

	_, err := p.Send(context.Background(), "ghost-message-text", nil, nil)
	if err == nil {
		t.Fatalf("Send() returned nil error; expected wrapped writeMessage failure")
	}
	if !errors.Is(err, errFakeWriteMessage) {
		t.Fatalf("Send() error = %v; want wrap of errFakeWriteMessage", err)
	}

	// The critical assertion: no user entry was appended.
	entries := p.eventLog.Entries()
	for _, e := range entries {
		if e.Type == "user" {
			t.Errorf("ghost user entry leaked into EventLog: %+v", e)
		}
	}
	if len(entries) > 0 {
		t.Logf("entries after failed Send: %d (must contain no Type==\"user\")", len(entries))
	}

	// Sanity: process state should have rolled back to Ready by Send's defer.
	if got := p.State(); got != StateReady {
		t.Errorf("post-fail state = %v; want StateReady", got)
	}
}
