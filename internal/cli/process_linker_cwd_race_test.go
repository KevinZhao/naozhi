package cli

import (
	"testing"
	"time"
)

// TestSetCwdForLinker_AfterTheReadLoopHasRun: the reconnect path starts the
// read loop before the router hands the linker its workspace, so the read
// loop may already have handled a system/init — and read the linker's
// project dir — when SetCwdForLinker writes it (run under -race).
func TestSetCwdForLinker_AfterTheReadLoopHasRun(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	p.InitLinker("") // SpawnReconnect has no cwd
	p.startReadLoop()
	defer func() {
		srv.Close()
		<-p.done
	}()

	srv.SendStdout(`{"type":"system","subtype":"init","session_id":"sess-reconnect"}`)
	// Let the loop handle the init. A timer, not a signal from the loop: any
	// synchronisation with it would order its read before the write below and
	// hide the race this test is for.
	<-time.After(100 * time.Millisecond)
	p.SetCwdForLinker("/srv/workspace")

	srv.SendStdout(`{"type":"system","subtype":"init","session_id":"sess-reconnect"}`)
	if got := p.linker.ParentSessionID(); got != "sess-reconnect" {
		t.Errorf("linker parent session = %q, want sess-reconnect", got)
	}
}
