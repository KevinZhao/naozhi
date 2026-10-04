package cli

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

func waitPassthroughState(t *testing.T, p *Process, want ProcessState) {
	t.Helper()
	testhelper.Eventually(t, func() bool { return p.State() == want }, 2*time.Second,
		"process state never reached "+want.String())
}

// awaitUnclaimedEvent blocks until readLoop delivers an unclaimed event of
// type typ to eventCh. An unowned result reaches eventCh before
// endUnownedTurn runs, so a state that follows from it must be polled, or
// read only after a later frame has been awaited (readLoop is sequential).
func awaitUnclaimedEvent(t *testing.T, p *Process, typ string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-p.eventCh:
			if ev.Type == typ {
				return
			}
		case <-deadline:
			t.Fatal("unclaimed " + typ + " never reached eventCh")
		}
	}
}

func sendPassthroughAsync(p *Process, text string) (<-chan *clievent.SendResult, <-chan error) {
	resCh := make(chan *clievent.SendResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := p.SendPassthrough(context.Background(), text, nil, nil, "")
		if err != nil {
			errCh <- err
			return
		}
		resCh <- res
	}()
	return resCh, errCh
}

func awaitPassthroughResult(t *testing.T, resCh <-chan *clievent.SendResult, errCh <-chan error, want string) {
	t.Helper()
	select {
	case res := <-resCh:
		if res.Text != want {
			t.Fatalf("result = %q, want %q", res.Text, want)
		}
	case err := <-errCh:
		t.Fatalf("SendPassthrough: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("SendPassthrough did not return")
	}
}

func (s *passthroughShim) emitAssistantText(text string) {
	s.srv.SendStdout(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]}}`)
}

// After a passthrough turn ends, the CLI wakes itself (background
// task-notification): init, assistant, result with no user message. The
// process must be Ready again once that result lands.
func TestPassthrough_CLIInitiatedTurnReturnsToReady(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	resCh, errCh := sendPassthroughAsync(sh.proc, "hello")
	in := sh.expectWrite(t, 2*time.Second)
	sh.emitInit("s1")
	sh.emitReplay(in.UUID, "hello")
	sh.emitResult("s1", "hi")
	awaitPassthroughResult(t, resCh, errCh, "hi")
	waitPassthroughState(t, sh.proc, StateReady)

	sh.emitInit("s1")
	waitPassthroughState(t, sh.proc, StateRunning)
	sh.emitAssistantText("background done")
	sh.emitResult("s1", "background done")
	awaitUnclaimedEvent(t, sh.proc, "result")
	waitPassthroughState(t, sh.proc, StateReady)
}

// A message queued during a CLI-initiated turn keeps the process Running past
// that turn's result; its own turn ends it.
func TestPassthrough_SendQueuedDuringCLIInitiatedTurn(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	sh.emitInit("s1")
	waitPassthroughState(t, sh.proc, StateRunning)
	sh.emitAssistantText("working")

	resCh, errCh := sendPassthroughAsync(sh.proc, "follow-up")
	in := sh.expectWrite(t, 2*time.Second)

	sh.emitResult("s1", "background done")
	awaitUnclaimedEvent(t, sh.proc, "result")
	sh.emitAssistantText("barrier")
	awaitUnclaimedEvent(t, sh.proc, "assistant")
	if got := sh.proc.State(); got != StateRunning {
		t.Fatalf("state with queued message = %v, want Running", got)
	}

	sh.emitInit("s1")
	sh.emitReplay(in.UUID, "follow-up")
	sh.emitResult("s1", "answer")
	awaitPassthroughResult(t, resCh, errCh, "answer")
	waitPassthroughState(t, sh.proc, StateReady)
}

// A message the CLI folds into its own turn claims that turn's result.
func TestPassthrough_SendMergedIntoCLIInitiatedTurn(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	sh.emitInit("s1")
	waitPassthroughState(t, sh.proc, StateRunning)

	resCh, errCh := sendPassthroughAsync(sh.proc, "follow-up")
	in := sh.expectWrite(t, 2*time.Second)
	sh.emitReplay(in.UUID, "follow-up")
	sh.emitResult("s1", "merged answer")
	awaitPassthroughResult(t, resCh, errCh, "merged answer")
	waitPassthroughState(t, sh.proc, StateReady)
}
