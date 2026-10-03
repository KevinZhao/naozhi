package cli

import (
	"sync"
	"sync/atomic"

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
	// sendOwned: a Send claimed the current turn (evSendBegin moved) and has
	// not returned, so a result belongs to it even when no passthrough slot
	// claimed it. Written only in transitionLocked.
	sendOwned bool

	interrupted    atomic.Bool // set by Interrupt(), cleared by next Send()
	interruptedRun atomic.Bool // true when Interrupt() was called while Running
	// reconnectedMidTurn: SpawnReconnect found a turn in flight, so a result
	// with no active Send ends that turn (one-shot, CAS-consumed).
	reconnectedMidTurn atomic.Bool
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
	}
	switch {
	case ev == evSendBegin && moved:
		t.sendOwned = true
	case ev == evSendEnd || ev == evDied:
		t.sendOwned = false
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
}
