package cli

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// The transcript's user and result entries carry the run they belong to
// (#3436), so the event log joins runhistory and the cost ledger by run id.

// entryRunIDs lists the RunID of every entry of kind in p's event log.
func entryRunIDs(p *Process, kind string) []string {
	var ids []string
	for _, e := range p.eventLog.Entries() {
		if e.Type == kind {
			ids = append(ids, e.RunID)
		}
	}
	return ids
}

func wantRunIDs(t *testing.T, p *Process, kind string, want ...string) {
	t.Helper()
	if got := entryRunIDs(p, kind); !slices.Equal(got, want) {
		t.Fatalf("%s entry run ids = %q, want %q", kind, got, want)
	}
}

// A merged passthrough turn: each prompt names its own run, the one result
// names the head run, which carries the cost.
func TestEventLogRunID_PassthroughMergedTurn(t *testing.T) {
	sh := startWatchdogShim(t, time.Hour, time.Hour, parkedWatchdog)
	_, outA := sh.sendAsync(t, runCtx("run-a"), "A")
	_, outB := sh.sendAsync(t, runCtx("run-b"), "B")
	sh.emitInit("s1")
	sh.emitReplay("cli-merged-uuid", "A B")
	sh.emitResult("s1", "merged")
	for name, out := range map[string]<-chan passthroughOut{"A": outA, "B": outB} {
		if o := waitOut(t, name, out, 3*time.Second); o.err != nil {
			t.Fatalf("%s: %v", name, o.err)
		}
	}
	wantRunIDs(t, sh.proc, clievent.KindUser, "run-a", "run-b")
	wantRunIDs(t, sh.proc, clievent.KindResult, "run-a")
}

// A legacy Send's prompt and result name its run. A stray result read once
// the process is Ready again, and a turn the CLI then starts itself, name
// none, though the turn state still holds the last Send's run.
func TestEventLogRunID_LegacySend(t *testing.T) {
	sh := startWatchdogShim(t, time.Hour, time.Hour, parkedWatchdog)
	booked := make(chan clievent.SendResult, 4)
	sh.proc.SetOnUnownedResult(func(r clievent.SendResult) { booked <- r })
	errCh := make(chan error, 1)
	go func() {
		_, err := sh.proc.Send(runCtx("run-l"), "hi", nil, nil)
		errCh <- err
	}()
	sh.expectWrite(t, 2*time.Second)
	sh.emitResult("s1", "ok")
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Send did not return")
	}
	wantRunIDs(t, sh.proc, clievent.KindUser, "run-l")
	wantRunIDs(t, sh.proc, clievent.KindResult, "run-l")

	sh.emitResult("s1", "stray")
	awaitBooked(t, booked, 0.001)
	sh.emitInit("s1")
	sh.emitResult("s1", "background turn")
	awaitBooked(t, booked, 0.001)
	wantRunIDs(t, sh.proc, clievent.KindResult, "run-l", "", "")
}

// The late result of a Send that gave up names it; a stray result after that
// one is booked, and a turn the CLI starts itself, name no run.
func TestEventLogRunID_AbandonedSend(t *testing.T) {
	sh := startWatchdogShim(t, time.Hour, time.Hour, parkedWatchdog)
	booked := make(chan clievent.SendResult, 4)
	sh.proc.SetOnUnownedResult(func(r clievent.SendResult) { booked <- r })

	ctx, cancel := context.WithCancel(runCtx("run-l"))
	errCh := make(chan error, 1)
	go func() {
		_, err := sh.proc.Send(ctx, "hi", nil, nil)
		errCh <- err
	}()
	sh.expectWrite(t, 2*time.Second)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Send err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Send did not return after cancel")
	}
	sh.emitResult("s1", "late reply")
	awaitBooked(t, booked, 0.001)
	sh.emitResult("s1", "stray duplicate")
	awaitBooked(t, booked, 0.001)
	sh.emitInit("s1")
	sh.emitResult("s1", "background turn")
	awaitBooked(t, booked, 0.001)

	wantRunIDs(t, sh.proc, clievent.KindUser, "run-l")
	wantRunIDs(t, sh.proc, clievent.KindResult, "run-l", "", "")
}

// The aborted result of an abandoned Send, read after the next Send claimed
// the process, names the abandoned run, not the live one.
func TestEventLogRunID_OrphanedAbortResult(t *testing.T) {
	_, lg := captureRunLogs(t)
	p := bareTurnProcess(t, make(chan clievent.Event, 4), "run-a")
	p.SetOnUnownedResult(func(clievent.SendResult) {})
	p.turn.abortRequested.arm()
	p.turn.mu.Lock()
	p.turn.sendAbandoned = true
	p.turn.mu.Unlock()
	p.transition(evSendEnd)
	if _, ok := p.turn.claimSend("run-b"); !ok {
		t.Fatal("second claim failed")
	}

	p.dispatchProtocolEvent(clievent.Event{Type: "result", SubType: "error_during_execution", SessionID: "s1"}, lg)
	wantRunIDs(t, p, clievent.KindResult, "run-a")
}
