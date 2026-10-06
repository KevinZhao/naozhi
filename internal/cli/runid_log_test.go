package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/ctxutil"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
)

// The cli logs that name a result nobody waits for carry the run it belongs
// to (#3436), so a grep by a dashboard run_id reaches its late spend.

// runLogSink is a goroutine-safe JSON log capture behind ctxutil.Handler.
type runLogSink struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *runLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// records returns the decoded records whose msg is msg.
func (s *runLogSink) records(msg string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(s.b.Bytes()), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// captureRunLogs routes slog.Default into a sink for the test's duration.
// Not for parallel tests: slog.Default is process-global.
func captureRunLogs(t *testing.T) (*runLogSink, *slog.Logger) {
	t.Helper()
	sink := &runLogSink{}
	lg := slog.New(ctxutil.NewHandler(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	prev := slog.Default()
	slog.SetDefault(lg)
	t.Cleanup(func() { slog.SetDefault(prev) })
	return sink, lg
}

const abandonedMsg = "cli: abandoned run's result booked as unowned"

func runCtx(id string) context.Context {
	return ctxutil.WithRunID(context.Background(), id)
}

// wantOneRecord fails unless exactly one msg record exists and carries runID.
func wantOneRecord(t *testing.T, sink *runLogSink, msg, runID string) map[string]any {
	t.Helper()
	recs := sink.records(msg)
	if len(recs) != 1 || recs[0]["run_id"] != runID {
		t.Fatalf("%q records = %v, want one with run_id %s", msg, recs, runID)
	}
	return recs[0]
}

func TestPassthroughRunID_MergedFanoutNamesEveryRun(t *testing.T) {
	sink, _ := captureRunLogs(t)
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
	rec := wantOneRecord(t, sink, "passthrough: fanout", "run-a")
	if ids, _ := rec["merged_run_ids"].([]any); len(ids) != 1 || ids[0] != "run-b" {
		t.Fatalf("merged_run_ids = %v, want [run-b]", rec["merged_run_ids"])
	}
}

// A slot whose caller left (ctx canceled, or orphaned by the bail timer)
// books its late result under the caller's run.
func TestPassthroughRunID_AbandonedSlotResultNamesItsRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		total   time.Duration
		cancel  bool
		wantErr error
	}{
		{"canceled", time.Hour, true, context.Canceled},
		{"bail orphaned", 80 * time.Millisecond, false, clierr.ErrOrphanedSlot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink, _ := captureRunLogs(t)
			sh := startWatchdogShim(t, time.Hour, tc.total, parkedWatchdog)
			booked := make(chan clievent.SendResult, 4)
			sh.proc.SetOnUnownedResult(func(r clievent.SendResult) { booked <- r })

			ctx, cancel := context.WithCancel(runCtx("run-a"))
			defer cancel()
			uuid, out := sh.sendAsync(t, ctx, "A")
			sh.emitInit("s1")
			sh.emitReplay(uuid, "A")
			if tc.cancel {
				cancel()
			}
			if o := waitOut(t, "A", out, 3*time.Second); !errors.Is(o.err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", o.err, tc.wantErr)
			}
			if !tc.cancel {
				wantOneRecord(t, sink, "passthrough: slot orphaned", "run-a")
			}
			sh.emitResult("s1", "late reply")
			awaitBooked(t, booked, 0.001)
			if rec := wantOneRecord(t, sink, abandonedMsg, "run-a"); rec["cli_session"] != "s1" {
				t.Fatalf("cli_session = %v, want s1", rec["cli_session"])
			}
		})
	}
}

// A result already delivered when the caller leaves is taken back and booked
// under that caller's run (the select picks either arm, so retry until the
// cancel arm wins).
func TestPassthroughRunID_ResultTakenBackOnCancelNamesItsRun(t *testing.T) {
	sink, _ := captureRunLogs(t)
	sh := startWatchdogShim(t, time.Hour, time.Hour, parkedWatchdog)
	var rec bookingRecorder
	sh.proc.SetOnUnownedResult(rec.book)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 200 {
		slot := newTestSlot(uint64(i + 1))
		slot.runID = "run-a"
		slot.resultCh <- &clievent.SendResult{CostUSD: 3, SessionID: "s1"}
		if _, err := sh.proc.awaitSlot(canceled, slot); err == nil {
			continue
		}
		if len(rec.take()) == 1 {
			wantOneRecord(t, sink, abandonedMsg, "run-a")
			return
		}
	}
	t.Fatal("ctx.Done arm never took a delivered result back in 200 tries")
}

// A legacy Send that gives up leaves its run on the process, and the result
// that arrives afterwards is logged under it; a turn the CLI then starts on
// its own is nobody's run.
func TestSendRunID_AbandonedSendResultNamesItsRun(t *testing.T) {
	sink, _ := captureRunLogs(t)
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
	if got := sh.proc.turn.currentRunID(); got != "run-l" {
		t.Fatalf("turn run id = %q, want run-l", got)
	}
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
	wantOneRecord(t, sink, abandonedMsg, "run-l")

	sh.emitInit("s1")
	sh.emitResult("s1", "background turn")
	awaitBooked(t, booked, 0.001)
	if recs := sink.records(abandonedMsg); len(recs) != 1 {
		t.Fatalf("a CLI-started turn was logged as an abandoned run's: %v", recs)
	}
}

// bareTurnProcess is a non-replay Process whose turn is claimed by run, with
// no readLoop behind it.
func bareTurnProcess(t *testing.T, eventCh chan clievent.Event, run string) *Process {
	t.Helper()
	p := &Process{eventLog: ring.NewEventLog(8), eventCh: eventCh, killCh: make(chan struct{})}
	p.transition(evReadLoopStart)
	if _, ok := p.turn.claimSend(run); !ok {
		t.Fatal("claimSend did not claim a Ready process")
	}
	return p
}

// The aborted result of a Send that gave up, read after the next Send claimed
// the process, is booked under the abandoned run, not the live one.
func TestReadLoopRunID_OrphanedAbortResultNamesTheAbandonedRun(t *testing.T) {
	sink, lg := captureRunLogs(t)
	p := bareTurnProcess(t, make(chan clievent.Event, 4), "run-a")
	var rec bookingRecorder
	p.SetOnUnownedResult(rec.book)
	p.turn.abortRequested.arm()
	p.turn.mu.Lock()
	p.turn.sendAbandoned = true
	p.turn.mu.Unlock()
	p.transition(evSendEnd)
	if _, ok := p.turn.claimSend("run-b"); !ok {
		t.Fatal("second claim failed")
	}

	p.dispatchProtocolEvent(clievent.Event{Type: "result", SubType: "error_during_execution", SessionID: "s1"}, lg)
	if got := rec.take(); len(got) != 1 {
		t.Fatalf("booked %d results, want the orphaned one", len(got))
	}
	if len(p.eventCh) != 0 {
		t.Fatal("the orphaned result reached the live Send")
	}
	wantOneRecord(t, sink, abandonedMsg, "run-a")
}

// A result readLoop cannot hand to the live Send names that Send's run.
func TestReadLoopRunID_DroppedResultNamesTheLiveSend(t *testing.T) {
	sink, lg := captureRunLogs(t)
	p := bareTurnProcess(t, make(chan clievent.Event), "run-x")
	p.dispatchProtocolEvent(clievent.Event{Type: "result", SubType: "success"}, lg)
	wantOneRecord(t, sink, "eventCh full, dropped result", "run-x")
}
