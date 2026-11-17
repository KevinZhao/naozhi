package cli

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// An abandoned turn's aborted result that lands after the next Send's turn
// began is only booked; when that Send then ends without its own result (CLI
// exit, watchdog), the ring fallback must not return the orphan as its answer.
func TestLegacySend_FallbackSkipsOrphanedResult(t *testing.T) {
	for _, path := range []string{"exit", "watchdog"} {
		t.Run(path, func(t *testing.T) {
			sh := newPassthroughShim(t)
			sh.proc.turn.state = StateReady
			if path == "watchdog" {
				sh.proc.noOutputTimeout = 300 * time.Millisecond
			}
			booked := make(chan clievent.SendResult, 8)
			sh.proc.SetOnUnownedResult(func(r clievent.SendResult) { booked <- r })
			t.Cleanup(sh.close)
			go sh.proc.readLoop()

			abandonInterruptedSend(t, sh)
			outB := startLateAbortSend(t, sh, context.Background(), "B")
			// Past B's turn start on the ring clock.
			time.Sleep(5 * time.Millisecond)
			sh.emitAssistantText("A-partial")
			sh.srv.SendStdout(abortedResult)
			select {
			case <-booked:
			case <-time.After(3 * time.Second):
				t.Fatal("orphaned result not booked")
			}
			if path == "exit" {
				sh.srv.Close()
			}

			o := awaitLateAbortSend(t, outB, "B")
			if o.err == nil {
				t.Fatalf("Send B = %+v, nil error; want the %s error, not the orphan's text", o.res, path)
			}
			if path == "watchdog" {
				testhelper.Eventually(t, func() bool { return !sh.proc.Alive() }, 3*time.Second, "watchdog kills the CLI")
			}
		})
	}
}
