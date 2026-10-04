package cli

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// turnState is the process's turn: its state machine, the CLI session ID, the
// turn-done hook, and the interrupt flags that must change together with the
// state they were read against. mu guards all of it. The flags are atomics so
// drainStaleEvents can Swap them, but every write holds mu.
type turnState struct {
	// mu: accessors use RLock so snapshot polls run in parallel; write paths
	// (transitions, Send's turn claim, Interrupt's snapshot-and-flag) use Lock.
	mu sync.RWMutex
	// state changes only through transition; Dead is terminal (nextState).
	state ProcessState
	// sessionID is the CLI's session ID. Readers use Process.SessionID, never
	// the field, to avoid racing readLoop's writes (#623).
	sessionID string
	// onTurnDone is called when a turn no Send owns ends (e.g. a shim
	// reconnect set Running but the CLI finished before any Send), and on
	// death, so the session layer can broadcast state changes. Read under mu
	// (assign via SetOnTurnDone), invoked after mu is released.
	//
	// Implementations MUST be idempotent: readLoop may fire it more than once
	// per turn from arms that run back-to-back — the result +
	// reconnectedMidTurn CAS path followed by <-killCh, plus cli_exited, the
	// fall-out Dead path and the panic defer.
	onTurnDone func()
	// onUnownedResult receives the result of a turn no Send owns — one the
	// CLI started itself (a background-task notification) — so the session
	// can book its cost; such a result never reaches a Send's finishRun.
	// Assign via SetOnUnownedResult; read under mu, invoked after release.
	onUnownedResult func(clievent.SendResult)

	interrupted    atomic.Bool // set by Interrupt(), cleared by next Send()
	interruptedRun atomic.Bool // true when Interrupt() was called while Running
	// abortRequested: naozhi asked the in-flight turn to stop (Interrupt,
	// InterruptViaControl, a priority:"now" passthrough send). readLoop takes
	// it onto the next result as Event.Aborted, so a consumer can tell that
	// abort's error_during_execution from a real failure.
	abortRequested abortMarker
	// reconnectedMidTurn: SpawnReconnect found a turn in flight, so a result
	// with no active Send ends that turn (one-shot, CAS-consumed).
	reconnectedMidTurn atomic.Bool
	// unowned: the current Running turn was entered by evTurnStarted, so no
	// Send's defer will end it (e.g. the CLI waking itself for a background
	// task-notification).
	unowned bool
}

// transition applies ev and reports the state before it and whether the state
// moved.
func (t *turnState) transition(ev stateEvent) (prev ProcessState, moved bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.transitionLocked(ev)
}

// transitionLocked is transition for a caller that holds mu.
func (t *turnState) transitionLocked(ev stateEvent) (prev ProcessState, moved bool) {
	prev = t.state
	next, moved := nextState(prev, ev)
	if moved {
		t.state = next
		t.unowned = ev == evTurnStarted
	}
	return prev, moved
}

// controlAcks matches control_request acks to their waiters: pending SetModel
// ack waiters keyed by request_id, registered before the wire write and
// removed by the waiter, so a late ack is dropped harmlessly
// (docs/rfc/dashboard-model-effort-control.md §4.4).
type controlAcks struct {
	mu      sync.Mutex
	waiters map[string]chan error
	// seq generates request_id suffixes for control_requests; the CLI only
	// echoes it back, so per-connection uniqueness suffices.
	seq atomic.Int64
}

// register installs a waiter for reqID. The channel receives nil (success)
// or an error carrying the CLI's rejection text; it is buffered so a delivery
// racing the waiter's timeout never blocks readLoop.
func (a *controlAcks) register(reqID string) chan error {
	ch := make(chan error, 1)
	a.mu.Lock()
	if a.waiters == nil {
		a.waiters = make(map[string]chan error, 1)
	}
	a.waiters[reqID] = ch
	a.mu.Unlock()
	return ch
}

// unregister removes reqID's waiter (deferred by SetModel so a late or never
// ack cannot leak the entry).
func (a *controlAcks) unregister(reqID string) {
	a.mu.Lock()
	delete(a.waiters, reqID)
	a.mu.Unlock()
}

// take removes and returns reqID's waiter, if any.
func (a *controlAcks) take(reqID string) (chan error, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch, ok := a.waiters[reqID]
	if ok {
		delete(a.waiters, reqID)
	}
	return ch, ok
}

// sendSlots is the passthrough slot machinery: with SendPassthrough, readLoop
// routes results through fanoutTurnResult instead of eventCh (legacy Send
// leaves pending nil). Lock ordering (docs/rfc/passthrough-mode.md §5.2.6):
//
//	link write lock → mu  (Send path; append slot + write stdin atomically)
//	mu alone              (readLoop, cancel, reconnect)
type sendSlots struct {
	mu      sync.Mutex
	pending []*sendSlot // FIFO by stdin write order
	current []*sendSlot // slots claimed by the in-flight turn
	idGen   atomic.Uint64
	// turnStartedAt is when the turn the CLI owes the head of pending began,
	// the passthrough total_timeout clock: set when a slot enters an empty
	// queue, reset by a result that leaves slots pending. Stale while pending
	// is empty.
	turnStartedAt time.Time
}

// abortMarker counts abort requests not yet matched by a result. Each arm has
// its own disarm, so a failed send rolls back only itself. A result takes the
// whole count: repeated aborts of one turn yield one result, and a leftover
// count would mark a later real failure Aborted. Hence at most one result per
// read is marked: if a second abort lands before the first abort's result is
// read, the second aborted turn's result is not marked.
type abortMarker struct{ n atomic.Int32 }

func (m *abortMarker) arm() { m.n.Add(1) }

// disarm undoes one arm, never below zero: a result may already have taken it.
func (m *abortMarker) disarm() {
	for {
		n := m.n.Load()
		if n <= 0 || m.n.CompareAndSwap(n, n-1) {
			return
		}
	}
}

func (m *abortMarker) take() bool  { return m.n.Swap(0) > 0 }
func (m *abortMarker) clear()      { m.n.Store(0) }
func (m *abortMarker) armed() bool { return m.n.Load() > 0 }
