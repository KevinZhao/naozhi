package turn

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// Priority is how urgently a request preempts the session's in-flight turn.
type Priority uint8

const (
	// PriorityNormal waits its turn: queued behind the active turn, or (in
	// ModePassthrough) handed to the CLI's own command queue.
	PriorityNormal Priority = iota
	// PriorityNow is /urgent: the CLI aborts the in-flight turn and runs this
	// message next. It never enters the queue.
	PriorityNow
)

// Request is one user message submitted to a session key.
type Request struct {
	Key      string
	Text     string
	Images   []clievent.Attachment
	Priority Priority
	// Origin is the entry point the message came from; it receives the
	// admission ack, the turn's outcome and any drop. A nil Origin is silent.
	Origin Origin
}

// Ack is Submit's immediate answer, also passed to Origin.Admitted.
type Ack uint8

const (
	// AckOwner: the session was idle; this request runs now and its caller's
	// Admission carries the owner loop that drains whatever queues behind it.
	AckOwner Ack = iota
	// AckQueued: the session was busy; the request waits in the queue and
	// joins the next merged turn.
	AckQueued
	// AckDetached: the request runs as its own turn outside the owner loop
	// (ModePassthrough, or PriorityNow).
	AckDetached
	// AckDropped: the session was busy and the queue is disabled (MaxDepth<=0).
	AckDropped
	// AckShuttingDown: the Admission declined the turn.
	AckShuttingDown
)

// DropReason says why a queued request will never get a turn.
type DropReason uint8

const (
	DropReset    DropReason = iota // Reset discarded the queue (/new, /clear)
	DropShutdown                   // the owner loop's ctx ended, or its Admission declined, with messages queued
	DropPanic                      // a turn on the key panicked and the queue was discarded
	DropEvicted                    // a newer message pushed it out of a full queue
	DropRemoved                    // the router retired the key (session removed, reset or closed) with messages queued
)

// Origin is an entry point's view of one submitted request. Every method is
// called from turn with no lock held.
type Origin interface {
	// Sink is the delivery identity: requests whose origins share a Sink get
	// one Begin per turn between them. IM uses "im:<platform>:<chat>", plus
	// "#<thread>" in a thread; a WS send "ws:<conn>:<sendID>", HTTP "http:<key>".
	Sink() string
	// Admitted reports Submit's Ack. It runs inside Submit; for AckOwner and
	// AckDetached that is before the turn's goroutine (if any) starts. An ack
	// that completes after Admitted returns must be ordered before its clear
	// by the origin (#1963). An AckQueued request may already be drained into
	// a running turn by then.
	Admitted(ctx context.Context, a Ack)
	// SessionOpts returns the options for GetOrCreate. Only the origin that
	// holds the owner loop (or runs a detached turn) is asked, once per turn.
	SessionOpts(key string) sessionview.AgentOpts
	// Begin opens this origin's delivery for one turn. It is called once per
	// receiving sink per turn; a nil Delivery receives nothing.
	Begin(ctx context.Context, t TurnInfo) Delivery
	// Dropped reports that the queued request will never get a turn.
	Dropped(ctx context.Context, why DropReason)
}

// Role is how a receiver relates to a turn's batch.
type Role uint8

const (
	// RoleHead: the origin's request is in the batch (first of its sink).
	RoleHead Role = iota
	// RoleObserver: the origin holds the owner loop but none of its sink's
	// requests are in this batch.
	RoleObserver
)

// TurnInfo describes one receiver of one turn.
type TurnInfo struct {
	Role Role
	// First is true for the turn that starts an owner loop, and for a
	// detached turn at PriorityNormal (each passthrough message stands alone).
	First bool
	// Mates are the batch's other origins with this receiver's sink, in
	// batch order. Their requests are answered by this delivery.
	Mates []Origin
	// Merged is the number of requests coalesced into the turn.
	Merged int
	// Primary marks the one receiver that answers for the turn as a whole:
	// the owner's sink in an owner-loop turn, the request in a detached one.
	// A per-turn side effect (a /health counter) belongs to it alone.
	Primary bool
}

// Delivery is one receiver's view of one turn, in call order: BeforeSession,
// SessionReady (only if GetOrCreate succeeded), Finish (exactly once, unless
// a hook of this delivery panicked).
type Delivery interface {
	// BeforeSession runs before GetOrCreate (IM: takeover on a First turn).
	BeforeSession(ctx context.Context)
	// SessionReady runs after GetOrCreate succeeds and may return a callback
	// for the turn's events; every receiver's callback sees every event.
	SessionReady(ctx context.Context, st sessionview.SessionStatus) clievent.EventCallback
	// Finish delivers the turn's outcome.
	Finish(ctx context.Context, o Outcome)
	// Blocking receivers are finished after Sender.AfterTurn, the others
	// before it, so a dashboard error reaches the client before the state
	// broadcast that would make it discard the error.
	Blocking() bool
}

// Stage is how far a turn got.
type Stage uint8

const (
	StageSession Stage = iota // GetOrCreate failed (or the turn panicked before it returned)
	StageSend                 // Send failed (or the turn panicked between GetOrCreate and Send returning)
	StageDone                 // Send succeeded
)

// Outcome is the result of one turn as delivered to each receiver.
type Outcome struct {
	Stage Stage
	Err   error
	// Panic is true when the turn panicked. The other fields are what the
	// turn had reached (Result is set once Send returned), so a receiver
	// checks Panic before Stage.
	Panic  bool
	Sess   Session
	Result *clievent.SendResult
}

// Session is the session a Sender hands back from GetOrCreate. turn passes
// it through to Sender.Send and Outcome without calling it; Backend is what
// the IM reply footer reads.
type Session interface {
	Backend() string
}

// SendSpec carries the per-turn send choices as explicit fields.
type SendSpec struct {
	// Passthrough asks for the CLI's concurrent passthrough path when the
	// session supports it; detached turns set it.
	Passthrough bool
	Priority    Priority
}

// Sender is the host's session side, implemented by server.
type Sender interface {
	GetOrCreate(ctx context.Context, key string, o sessionview.AgentOpts) (Session, sessionview.SessionStatus, error)
	Send(ctx context.Context, key string, s Session, text string, img []clievent.Attachment, spec SendSpec, onEvent clievent.EventCallback) (*clievent.SendResult, error)
	// AfterTurn broadcasts the session's post-turn state; it runs once per
	// turn whose Send returned, even if delivery then panicked.
	AfterTurn(key string)
	// Interrupt aborts the in-flight turn (ModeInterrupt's first follow-up).
	Interrupt(key string) sessionview.InterruptOutcome
	// DiscardPending fails the session's in-flight passthrough sends.
	DiscardPending(key string, reason error)
	Reset(key string, discardOverride bool)
	// NotifyIdle runs when an owner loop exits, after any panic is handled.
	NotifyIdle()
}

// RunKind is the kind of turn an Admission is asked to run.
type RunKind uint8

const (
	RunOwner    RunKind = iota // an owner loop: the first turn plus every drain after it
	RunDetached                // one detached turn
)

// Admission is the entry point's policy for running turns. Admit reserves a
// slot for one run of kind; ok=false declines it (shutdown). On ok, turn
// calls start exactly once: start runs fn inline or on a new goroutine, with
// the ctx the turn should use, and frees the slot when fn returns. The split
// lets Submit report Origin.Admitted after the decision and before fn runs.
type Admission interface {
	Admit(kind RunKind) (start func(fn func(ctx context.Context)), ok bool)
}
