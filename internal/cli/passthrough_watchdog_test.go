package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// passthroughOut is one SendPassthrough return.
type passthroughOut struct {
	res *clievent.SendResult
	err error
}

// setWatchdogTiming shortens the package-level watchdog knobs for one test.
// Tests calling it must not run in parallel.
func setWatchdogTiming(t *testing.T, minInterval, bailGrace time.Duration) {
	t.Helper()
	oldIv, oldGrace := watchdogMinCheckInterval, passthroughBailGrace
	watchdogMinCheckInterval, passthroughBailGrace = minInterval, bailGrace
	t.Cleanup(func() { watchdogMinCheckInterval, passthroughBailGrace = oldIv, oldGrace })
}

// startWatchdogShim returns a passthrough shim whose Process runs with the
// given budgets, its readLoop started.
func startWatchdogShim(t *testing.T, noOutput, total time.Duration) *passthroughShim {
	t.Helper()
	sh := newPassthroughShim(t)
	sh.proc.noOutputTimeout = noOutput
	sh.proc.totalTimeout = total
	t.Cleanup(sh.close)
	go sh.proc.readLoop()
	return sh
}

// sendAsync starts SendPassthrough(text) and waits for its stdin write, so
// successive calls queue in order. Returns the write's uuid and the outcome.
func (s *passthroughShim) sendAsync(t *testing.T, ctx context.Context, text string) (string, <-chan passthroughOut) {
	t.Helper()
	out := make(chan passthroughOut, 1)
	go func() {
		res, err := s.proc.SendPassthrough(ctx, text, nil, nil, "")
		out <- passthroughOut{res, err}
	}()
	return s.expectWrite(t, 2*time.Second).UUID, out
}

// waitOut returns the outcome on out or fails after d.
func waitOut(t *testing.T, name string, out <-chan passthroughOut, d time.Duration) passthroughOut {
	t.Helper()
	select {
	case o := <-out:
		return o
	case <-time.After(d):
		t.Fatalf("%s: SendPassthrough did not return within %v", name, d)
		return passthroughOut{}
	}
}

// waitDead waits for readLoop to finish tearing the process down.
func waitDead(t *testing.T, p *Process) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("process still alive 3s after the watchdog kill")
	}
}

// A CLI that goes silent mid-turn is killed after no_output_timeout, and
// every queued caller (the claimed head and the one queued behind it) gets
// the classified ErrNoOutputTimeout instead of waiting for the bail timer.
func TestPassthroughWatchdog_NoOutputKillsAndClassifiesEveryCaller(t *testing.T) {
	setWatchdogTiming(t, 5*time.Millisecond, 30*time.Second)
	sh := startWatchdogShim(t, 80*time.Millisecond, time.Minute)

	uuidA, outA := sh.sendAsync(t, context.Background(), "A")
	_, outB := sh.sendAsync(t, context.Background(), "B")
	sh.emitInit("s1")
	sh.emitReplay(uuidA, "A")

	for name, out := range map[string]<-chan passthroughOut{"A": outA, "B": outB} {
		o := waitOut(t, name, out, 3*time.Second)
		if !errors.Is(o.err, clierr.ErrNoOutputTimeout) {
			t.Errorf("%s: err = %v, want ErrNoOutputTimeout", name, o.err)
		}
	}
	waitDead(t, sh.proc)
	if got := sh.proc.DeathReason(); got != DeathReasonNoOutputTimeout {
		t.Errorf("DeathReason = %q, want %q", got, DeathReasonNoOutputTimeout)
	}
	if got := sh.proc.State(); got != StateDead {
		t.Errorf("State = %s, want Dead", got)
	}
	if sh.proc.PassthroughDepth() != 0 {
		t.Errorf("PassthroughDepth = %d after the kill, want 0", sh.proc.PassthroughDepth())
	}
}

// A CLI that keeps streaming but never finishes its turn is killed at
// total_timeout with ErrTotalTimeout: output resets the no-output clock but
// not the turn's total budget.
func TestPassthroughWatchdog_TotalTimeoutDespiteOutput(t *testing.T) {
	setWatchdogTiming(t, 5*time.Millisecond, 30*time.Second)
	sh := startWatchdogShim(t, time.Second, 200*time.Millisecond)

	uuidA, outA := sh.sendAsync(t, context.Background(), "A")
	sh.emitInit("s1")
	sh.emitReplay(uuidA, "A")

	streamed := make(chan struct{})
	go func() {
		defer close(streamed)
		sh.streamFor(time.Minute)
	}()

	o := waitOut(t, "A", outA, 3*time.Second)
	if !errors.Is(o.err, clierr.ErrTotalTimeout) {
		t.Errorf("err = %v, want ErrTotalTimeout", o.err)
	}
	waitDead(t, sh.proc)
	<-streamed
	if got := sh.proc.DeathReason(); got != DeathReasonTotalTimeout {
		t.Errorf("DeathReason = %q, want %q", got, DeathReasonTotalTimeout)
	}
}

// streamFor plays a turn body: an assistant event every 20ms for d, or until
// the process dies.
func (s *passthroughShim) streamFor(d time.Duration) {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	end := time.After(d)
	for {
		select {
		case <-end:
			return
		case <-s.proc.done:
			return
		case <-tick.C:
			s.emitAssistantText("working")
		}
	}
}

// runTwoQueuedTurns queues A and B, then plays two healthy turns of turnDur
// each, B's starting when A's result lands. B finishes about 2×turnDur after
// it was enqueued.
func runTwoQueuedTurns(t *testing.T, sh *passthroughShim, turnDur time.Duration) (outA, outB <-chan passthroughOut) {
	t.Helper()
	uuidA, outA := sh.sendAsync(t, context.Background(), "A")
	uuidB, outB := sh.sendAsync(t, context.Background(), "B")
	sh.emitInit("s1")
	sh.emitReplay(uuidA, "A")
	sh.streamFor(turnDur)
	sh.emitResult("s1", "done A")
	sh.emitInit("s1")
	sh.emitReplay(uuidB, "B")
	sh.streamFor(turnDur)
	sh.emitResult("s1", "done B")
	return outA, outB
}

// total_timeout is a per-turn budget: B ran ~2×turnDur from enqueue but only
// turnDur of its own turn, so the watchdog must not kill it.
func TestPassthroughWatchdog_TotalBudgetIsPerTurn(t *testing.T) {
	setWatchdogTiming(t, 5*time.Millisecond, 30*time.Second)
	sh := startWatchdogShim(t, 200*time.Millisecond, 400*time.Millisecond)

	outA, outB := runTwoQueuedTurns(t, sh, 250*time.Millisecond)
	for name, out := range map[string]<-chan passthroughOut{"A": outA, "B": outB} {
		o := waitOut(t, name, out, 3*time.Second)
		if o.err != nil || o.res == nil || o.res.Text != "done "+name {
			t.Errorf("%s: (%v, %v), want result %q", name, o.res, o.err, "done "+name)
		}
	}
	if !sh.proc.Alive() || sh.proc.DeathReason() != "" {
		t.Errorf("process killed (alive=%v reason=%q), want alive", sh.proc.Alive(), sh.proc.DeathReason())
	}
}

// The bail backstop measures the turn the CLI owes, not the slot's age: a
// slot queued behind a healthy turn is not orphaned. The watchdog is parked
// (no-output budget 1h, so its first tick is 30s out) and only bail can act.
func TestPassthroughBail_QueuedSlotIsNotOrphaned(t *testing.T) {
	setWatchdogTiming(t, time.Hour, 0)
	sh := startWatchdogShim(t, time.Hour, 400*time.Millisecond)

	outA, outB := runTwoQueuedTurns(t, sh, 250*time.Millisecond)
	for name, out := range map[string]<-chan passthroughOut{"A": outA, "B": outB} {
		o := waitOut(t, name, out, 3*time.Second)
		if o.err != nil || o.res == nil || o.res.Text != "done "+name {
			t.Errorf("%s: (%v, %v), want result %q", name, o.res, o.err, "done "+name)
		}
	}
}

// With the watchdog parked, a turn that overruns totalTimeout+grace still
// unblocks its caller with ErrOrphanedSlot: the backstop stays a backstop.
func TestPassthroughBail_StillFiresWhenTheWatchdogDoesNot(t *testing.T) {
	setWatchdogTiming(t, time.Hour, 0)
	sh := startWatchdogShim(t, time.Hour, 80*time.Millisecond)

	uuidA, outA := sh.sendAsync(t, context.Background(), "A")
	sh.emitInit("s1")
	sh.emitReplay(uuidA, "A")
	o := waitOut(t, "A", outA, 3*time.Second)
	if !errors.Is(o.err, clierr.ErrOrphanedSlot) {
		t.Errorf("err = %v, want ErrOrphanedSlot", o.err)
	}
}

// A canceled caller's tombstone still means the CLI owes a result, so the
// next sender behind a silent CLI gets ErrNoOutputTimeout instead of joining
// a queue that never drains.
func TestPassthroughWatchdog_TombstoneQueueIsNotIdle(t *testing.T) {
	setWatchdogTiming(t, 5*time.Millisecond, 30*time.Second)
	sh := startWatchdogShim(t, 80*time.Millisecond, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	_, outA := sh.sendAsync(t, ctx, "A")
	cancel()
	if o := waitOut(t, "A", outA, 3*time.Second); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("A: err = %v, want context.Canceled", o.err)
	}

	_, outB := sh.sendAsync(t, context.Background(), "B")
	o := waitOut(t, "B", outB, 3*time.Second)
	if !errors.Is(o.err, clierr.ErrNoOutputTimeout) {
		t.Errorf("B: err = %v, want ErrNoOutputTimeout", o.err)
	}
	waitDead(t, sh.proc)
}

// A CLI idle for longer than no_output_timeout before a send is not stalled:
// the no-output clock starts with the message that enters the empty queue.
func TestPassthroughWatchdog_IdleSilenceBeforeTheSendIsNotAStall(t *testing.T) {
	setWatchdogTiming(t, 5*time.Millisecond, 30*time.Second)
	sh := startWatchdogShim(t, 200*time.Millisecond, time.Minute)
	sh.proc.markOutput(time.Now().Add(-time.Hour))

	uuidA, outA := sh.sendAsync(t, context.Background(), "A")
	<-time.After(100 * time.Millisecond) // the CLI takes a while to start
	sh.emitInit("s1")
	sh.emitReplay(uuidA, "A")
	sh.emitResult("s1", "done A")
	if o := waitOut(t, "A", outA, 3*time.Second); o.err != nil {
		t.Fatalf("A: err = %v, want its result", o.err)
	}
}

// The kill hands every live queued slot the classified error itself rather
// than leaving them to readLoop's ErrProcessExited; a canceled tombstone is
// skipped but still keeps the queue owed.
func TestPassthroughWatchdogTick_ClassifiesEverySlot(t *testing.T) {
	sh := newPassthroughShim(t)
	t.Cleanup(sh.close)
	p := sh.proc
	newSlot := func(canceled bool) *sendSlot {
		s := &sendSlot{resultCh: make(chan *clievent.SendResult, 1), errCh: make(chan error, 1)}
		s.canceled.Store(canceled)
		return s
	}
	tomb, live1, live2 := newSlot(true), newSlot(false), newSlot(false)
	now := time.Now()

	// Idle queue with a stale clock: nothing is owed, nothing happens.
	p.markOutput(now.Add(-time.Hour))
	p.slots.turnStartedAt = now.Add(-time.Hour)
	if err := p.passthroughWatchdogTick(now, time.Second, time.Hour); err != nil {
		t.Fatalf("empty queue: err = %v, want nil", err)
	}

	p.slots.pending = []*sendSlot{tomb, live1, live2}
	if err := p.passthroughWatchdogTick(now, 2*time.Hour, 2*time.Hour); err != nil {
		t.Fatalf("within budget: err = %v, want nil", err)
	}
	if !p.Alive() || p.PassthroughDepth() != 3 {
		t.Fatalf("within budget: alive=%v depth=%d, want untouched", p.Alive(), p.PassthroughDepth())
	}

	err := p.passthroughWatchdogTick(now, time.Second, time.Hour)
	if !errors.Is(err, clierr.ErrNoOutputTimeout) {
		t.Fatalf("err = %v, want ErrNoOutputTimeout", err)
	}
	for i, s := range []*sendSlot{live1, live2} {
		select {
		case got := <-s.errCh:
			if !errors.Is(got, clierr.ErrNoOutputTimeout) {
				t.Errorf("live slot %d: err = %v, want ErrNoOutputTimeout", i, got)
			}
		default:
			t.Errorf("live slot %d: no error delivered by the kill", i)
		}
	}
	if len(tomb.errCh) != 0 {
		t.Error("canceled tombstone got an error; nobody waits on it")
	}
	select {
	case <-p.killCh:
	default:
		t.Error("Kill not issued")
	}
	if got := p.DeathReason(); got != DeathReasonNoOutputTimeout {
		t.Errorf("DeathReason = %q, want %q", got, DeathReasonNoOutputTimeout)
	}
	if p.PassthroughDepth() != 0 {
		t.Errorf("PassthroughDepth = %d, want 0", p.PassthroughDepth())
	}
}

func TestTurnDeadlineVerdict(t *testing.T) {
	t.Parallel()
	start := time.Now()
	const noOut, total = 10 * time.Second, time.Minute
	cases := []struct {
		name       string
		now, last  time.Duration // offsets from start
		wantReason string
		wantErr    error
	}{
		{"fresh", 5 * time.Second, 0, "", nil},
		{"silent just under", 10*time.Second - 1, 0, "", nil},
		{"silent at budget", 10 * time.Second, 0, DeathReasonNoOutputTimeout, clierr.ErrNoOutputTimeout},
		{"streaming within total", 59 * time.Second, 55 * time.Second, "", nil},
		{"streaming past total", time.Minute, 55 * time.Second, DeathReasonTotalTimeout, clierr.ErrTotalTimeout},
		{"both: no-output wins", 2 * time.Minute, 0, DeathReasonNoOutputTimeout, clierr.ErrNoOutputTimeout},
	}
	for _, c := range cases {
		reason, err := turnDeadlineVerdict(start.Add(c.now), start, start.Add(c.last), noOut, total)
		if reason != c.wantReason || !errors.Is(err, c.wantErr) || (c.wantErr == nil) != (err == nil) {
			t.Errorf("%s: (%q, %v), want (%q, %v)", c.name, reason, err, c.wantReason, c.wantErr)
		}
	}
}

func TestWatchdogCheckInterval(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ noOut, want time.Duration }{
		{2 * time.Second, time.Second},
		{20 * time.Second, 5 * time.Second},
		{10 * time.Minute, 30 * time.Second},
	} {
		if got := watchdogCheckInterval(c.noOut); got != c.want {
			t.Errorf("watchdogCheckInterval(%v) = %v, want %v", c.noOut, got, c.want)
		}
	}
}

// Send shares turnDeadlineVerdict: a silent turn dies of no-output and a
// streaming one of the total budget, each with its own death reason.
func TestSendWatchdog_SharedVerdict(t *testing.T) {
	setWatchdogTiming(t, 5*time.Millisecond, 30*time.Second)
	for _, c := range []struct {
		name            string
		noOutput, total time.Duration
		stream          bool
		wantErr         error
		wantReason      string
	}{
		{"silent", 80 * time.Millisecond, time.Minute, false, clierr.ErrNoOutputTimeout, DeathReasonNoOutputTimeout},
		{"streaming", time.Second, 200 * time.Millisecond, true, clierr.ErrTotalTimeout, DeathReasonTotalTimeout},
	} {
		t.Run(c.name, func(t *testing.T) {
			sh := newPassthroughShim(t)
			sh.proc.noOutputTimeout, sh.proc.totalTimeout = c.noOutput, c.total
			sh.proc.turn.state = StateReady
			t.Cleanup(sh.close)
			go sh.proc.readLoop()

			errCh := make(chan error, 1)
			go func() {
				_, err := sh.proc.Send(context.Background(), "hi", nil, nil)
				errCh <- err
			}()
			sh.expectWrite(t, 2*time.Second)
			if c.stream {
				go sh.streamFor(time.Minute)
			}
			select {
			case err := <-errCh:
				if !errors.Is(err, c.wantErr) {
					t.Errorf("err = %v, want %v", err, c.wantErr)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Send did not return")
			}
			waitDead(t, sh.proc)
			if got := sh.proc.DeathReason(); got != c.wantReason {
				t.Errorf("DeathReason = %q, want %q", got, c.wantReason)
			}
		})
	}
}
