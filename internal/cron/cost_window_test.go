package cron

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/costledger"
)

// windowSession books spend the way session.ManagedSession does: a result
// reported while the cost window is open is the run's, any other result and
// every process-end partial is a session row. CostTotals covers all of it, so
// a run measured by differencing totals would also charge the session's rows.
type windowSession struct {
	mu       sync.Mutex
	log      *[]string
	send     func(w *windowSession) (SendResult, error)
	afterEnd func(w *windowSession)
	open     bool
	window   float64
	total    float64
	rows     []float64
}

func (w *windowSession) note(ev string) {
	w.mu.Lock()
	*w.log = append(*w.log, ev)
	w.mu.Unlock()
}

// result books a result frame's spend.
func (w *windowSession) result(usd float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total += usd
	if w.open {
		w.window += usd
		return
	}
	w.rows = append(w.rows, usd)
}

// partial books a process-end estimate: always a session row.
func (w *windowSession) partial(usd float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total += usd
	w.rows = append(w.rows, usd)
}

func (w *windowSession) Send(context.Context, string) (SendResult, error) { return w.send(w) }
func (w *windowSession) SessionID() string                                { return "sess-w" }
func (w *windowSession) InterruptViaControl() InterruptOutcome            { return InterruptUnsupported }

func (w *windowSession) BeginCostWindow() {
	w.note("begin")
	w.mu.Lock()
	w.open, w.window = true, 0
	w.mu.Unlock()
}

func (w *windowSession) EndCostWindow() costledger.Increment {
	w.note("end")
	w.mu.Lock()
	was, inc := w.open, w.window
	w.open, w.window = false, 0
	w.mu.Unlock()
	if w.afterEnd != nil {
		w.afterEnd(w)
	}
	if !was {
		return costledger.Increment{}
	}
	return costledger.Increment{USD: inc}
}

func (w *windowSession) CostTotals() costledger.Totals {
	w.mu.Lock()
	defer w.mu.Unlock()
	return costledger.Totals{USD: w.total}
}

func (w *windowSession) sessionRows() []float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.rows)
}

// windowRouter logs Reset and ReleaseProcess into the session's call log and
// runs onReset, where a killed process would book its end.
type windowRouter struct {
	sess    *windowSession
	onReset func()
}

func (r windowRouter) RegisterCronStubWithChain(string, string, string, []string) {}
func (r windowRouter) Reset(string) {
	r.sess.note("reset")
	if r.onReset != nil {
		r.onReset()
	}
}
func (r windowRouter) GetOrCreate(context.Context, string, AgentOpts) (Session, SessionStatus, error) {
	return r.sess, SessionExisting, nil
}
func (r windowRouter) ReleaseProcess(string) bool { r.sess.note("release"); return true }

func newWindowSession(send func(w *windowSession) (SendResult, error)) *windowSession {
	return &windowSession{log: new([]string), send: send}
}

func cronAmounts(t *testing.T, l *costledger.Store) []float64 {
	t.Helper()
	var out []float64
	for _, e := range ledgerEntries(t, l) {
		if e.Source != costledger.SourceCronLocal {
			t.Fatalf("entry = %+v, want only cron rows", e)
		}
		out = append(out, e.Amount)
	}
	slices.Sort(out)
	return out
}

// A timed-out fresh run closes its window before the Reset that kills the
// turn: the run is charged what its results reported, and the kill's partial
// is the session's row alone.
func TestLocalRun_DeadlineClosesTheWindowBeforeReset(t *testing.T) {
	sess := newWindowSession(func(w *windowSession) (SendResult, error) {
		w.result(0.4)
		return SendResult{}, fmt.Errorf("send: %w", context.DeadlineExceeded)
	})
	var resets int
	router := windowRouter{sess: sess}
	router.onReset = func() {
		if resets++; resets > 1 { // the first Reset is the fresh preflight's
			sess.partial(0.25)
		}
	}
	s, ledger := newCostScheduler(t, router)
	j := &Job{ID: "0123456789abcdef", Schedule: "@every 5m", Prompt: "ping", FreshContext: true}
	s.putJobForTest(j)

	s.executeOpt(j.ID, true)

	log := *sess.log
	b := slices.Index(log, "begin")
	if b < 0 || !slices.Equal(log[b:], []string{"begin", "end", "reset"}) {
		t.Fatalf("calls = %v, want the window closed before the deadline Reset", log)
	}
	if got := cronAmounts(t, ledger); !slices.Equal(got, []float64{0.4}) {
		t.Fatalf("cron rows = %v, want [0.4]", got)
	}
	if rows := sess.sessionRows(); !slices.Equal(rows, []float64{0.25}) {
		t.Fatalf("session rows = %v, want the kill's partial", rows)
	}
}

// A process-end partial booked while the run's Send is live is a session row,
// so the run's increment must not carry it as well.
func TestLocalRun_PartialInsideTheWindowIsNotTheRuns(t *testing.T) {
	sess := newWindowSession(func(w *windowSession) (SendResult, error) {
		w.result(0.4)
		w.partial(0.25)
		return SendResult{Text: "done", SessionID: "sess-w"}, nil
	})
	s, ledger := newCostScheduler(t, windowRouter{sess: sess})
	j := &Job{ID: "0123456789abcdef", Schedule: "@every 5m", Prompt: "ping"}
	s.putJobForTest(j)

	s.executeOpt(j.ID, true)

	if got := cronAmounts(t, ledger); !slices.Equal(got, []float64{0.4}) {
		t.Fatalf("cron rows = %v, want [0.4] (0.65 counts the partial twice)", got)
	}
}

// An operator cancel leaves a persistent job's process alive; the cancelled
// turn's result arriving after the window closed is the session's row, in
// neither run's increment.
func TestLocalRun_PersistentCancelLateResultIsTheSessions(t *testing.T) {
	runs := 0
	sess := newWindowSession(func(w *windowSession) (SendResult, error) {
		runs++
		if runs == 1 {
			w.result(0.1)
			return SendResult{}, context.Canceled
		}
		w.result(0.3)
		return SendResult{Text: "done", SessionID: "sess-w"}, nil
	})
	sess.afterEnd = func(w *windowSession) {
		if runs == 1 {
			w.result(0.2) // the late result, before finishRun releases the gate
		}
	}
	s, ledger := newCostScheduler(t, windowRouter{sess: sess})
	j := &Job{ID: "0123456789abcdef", Schedule: "@every 5m", Prompt: "ping"}
	s.putJobForTest(j)

	s.executeOpt(j.ID, true)
	s.executeOpt(j.ID, true)

	if got := cronAmounts(t, ledger); len(got) != 2 || !near(got[0], 0.1) || !near(got[1], 0.3) {
		t.Fatalf("cron rows = %v, want [0.1 0.3]", got)
	}
	if rows := sess.sessionRows(); len(rows) != 1 || !near(rows[0], 0.2) {
		t.Fatalf("session rows = %v, want the late 0.2", rows)
	}
}

// A Send that panics still closes the window, or the session would keep
// collecting spend no run books.
func TestLocalRun_SendPanicClosesTheWindow(t *testing.T) {
	sess := newWindowSession(func(*windowSession) (SendResult, error) { panic("send blew up") })
	s, _ := newCostScheduler(t, windowRouter{sess: sess})
	j := &Job{ID: "0123456789abcdef", Schedule: "@every 5m", Prompt: "ping"}
	s.putJobForTest(j)

	func() {
		defer func() { _ = recover() }()
		s.executeOpt(j.ID, true)
	}()

	sess.mu.Lock()
	open := sess.open
	sess.mu.Unlock()
	if open || !slices.Contains(*sess.log, "end") {
		t.Fatalf("window open=%v after a panicking Send; calls %v", open, *sess.log)
	}
}
