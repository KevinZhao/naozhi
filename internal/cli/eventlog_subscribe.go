// File eventlog_subscribe.go: the subscriber broadcast leg — Subscribe(New), notifySubscribers,
// CloseSubscribers, and the EventSubscription handle (guarded by subMu,
// independent of the ring-buffer l.mu).
// Split from eventlog.go per docs/rfc/eventlog-split.md (ARCH-EVENTLOG-SPLIT);
// the EventLog struct and constructor live in eventlog.go.

package cli

import "sync"

// subscriber is the per-Subscribe handle. R229-PERF-12 reviewer
// suggested pooling these via sync.Pool to dodge alloc on dashboard
// reconnect spurts; that won't work because (a) closed channels cannot
// be re-used (the unsub path calls close(sub.ch), and reusing a closed
// channel makes a recv permanently return the zero value before any
// future notify lands), and (b) sync.Once cannot be reset to its
// pristine state without unsafe pointer surgery. The cheap path is
// `make(chan struct{}, 1) + sync.Once{}` per Subscribe — both are tiny
// allocations and tab-reload is human-cadence, so pooling buys ~2
// alloc/subscribe at most. Documented to discourage future
// "obvious" pool attempts that would silently break the close-once
// invariant.
type subscriber struct {
	ch        chan struct{} // buffered(1)
	closeOnce sync.Once

	// mu guards the send-vs-close race on ch. notifySubscribers takes it
	// (read) to perform the non-blocking send while observing `closed`;
	// the unsub / CloseSubscribers close path takes it (write) to flip
	// `closed` and close ch atomically. This per-subscriber lock lets
	// notifySubscribers snapshot l.subscribers under subMu.RLock and then
	// RELEASE subMu before the send loop (R20260603000023-PERF-4 / #1647):
	// concurrent notify waves across sessions — and multiple tabs on one
	// session — no longer serialise on the shared subMu for the whole loop.
	// The previous design held subMu.RLock across the full send loop solely
	// to block the close from racing the send; that responsibility now lives
	// on this fine-grained per-subscriber lock instead.
	mu     sync.RWMutex
	closed bool
}

// signal performs the non-blocking wake send under the subscriber's read
// lock, observing `closed` so it can never send on a closed channel. Returns
// without sending if the subscriber has already been closed.
func (s *subscriber) signal() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// close flips `closed` and closes ch under the write lock, exactly once.
// Mutually exclusive with signal() so an in-flight notify can never observe
// closed==false and then race the close into a send-on-closed panic.
func (s *subscriber) close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.ch)
		s.mu.Unlock()
	})
}

// eventLogClosedCh is a process-wide pre-closed `chan struct{}` returned by
// Subscribe when the EventLog has already been torn down (subsClosed=true).
// R247-PERF-14 (#553): late-arriving subscribers during dashboard reconnect
// storms used to allocate a fresh subscriber struct + buffered channel
// pair just to immediately close it; sharing one pre-closed channel skips
// both allocations on every post-close Subscribe. Receiving from a closed
// channel always returns the zero value with ok=false, so the caller's
// `select case <-notify: ok=false` arm still fires identically — and
// because the channel has no senders, it is permanently safe to share.
var eventLogClosedCh = func() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// notifySubscribers wakes all subscriber channels non-blockingly.
//
// Snapshot-then-unlock: subMu.RLock is held only long enough to copy the
// `l.subscribers` slice header into a local; it is released BEFORE the
// per-subscriber send loop. Each send goes through (*subscriber).signal,
// which takes that subscriber's own RWMutex (read) and observes its `closed`
// flag, so a concurrent close (unsub / CloseSubscribers) can never race the
// send into a send-on-closed-chan panic. The fine-grained per-subscriber
// lock replaces the old "hold subMu.RLock across the whole loop" guard.
//
// Fast path: idle sessions (no dashboard clients) check an atomic counter
// and skip subMu entirely. Append is invoked per content block on every
// stream-json event, so shaving one lock per assistant turn matters when
// N sessions run unattended. R65-PERF-M-1 upgraded from Mutex to RWMutex so
// concurrent notify calls from different sessions no longer serialise.
//
// R239-PERF-9 (2026-05-24): subscribers storage migrated from
// map[*subscriber]struct{} to []*subscriber. The hot iter dropped
// mapiterinit/mapiternext (~tens of ns/call × 25K calls/s = measurable
// in 500-session deployments) for a tight slice range. Unsubscribe is
// the cold path (one alloc/subscribe per session lifetime) and pays
// an O(N) scan to find + swap-to-end the leaving subscriber. closeOnce
// on subscriber.ch keeps the "close exactly once" invariant safe across
// the unsub-vs-CloseSubscribers race.
//
// R20260603000023-PERF-4 (#1647): the previous design held subMu.RLock for
// the ENTIRE send loop, which serialised same-session multi-tab dispatch:
// every Append on a hot session walked each subscriber in turn while holding
// the shared lock, blocking unsub.Lock until the loop ended. The slice header
// is now snapshotted under a short RLock and the lock dropped before the loop;
// the close-vs-send safety that the RLock used to provide moved onto the
// per-subscriber (*subscriber).mu (see the DO-NOT note that #455 left here —
// its prescribed fix, "move the close under a lock observed before send", is
// exactly what (*subscriber).signal / (*subscriber).close now implement). A
// local snapshot may include a subscriber that unsub removes from the slice
// mid-loop; signal() on that subscriber is harmless — it either sends a wake
// the now-detached reader ignores, or no-ops because close already ran.
func (l *EventLog) notifySubscribers() {
	if l.subCount.Load() == 0 {
		return
	}
	l.subMu.RLock()
	subs := l.subscribers
	l.subMu.RUnlock()
	for _, sub := range subs {
		sub.signal()
	}
}

// EventSubscription wraps an EventLog notification channel together with the
// matching cancel func so callers no longer pass a bare `<-chan struct{}`
// across package boundaries. R246-ARCH-12 / #792 (P0 subset): the raw channel
// surface forced every cross-package consumer (server/wshub, upstream
// connector, session/managed) to internalize EventLog's close semantics --
// "channel closed exactly once by either Cancel or CloseSubscribers, do not
// close it yourself, do not assume close ⇒ unsubscribe". Bundling the two
// into one value gives the eventlog package a single point of contact and
// lets the close contract stay an internal invariant of (*subscriber).closeOnce.
//
// Hot-path callers still consume Notify() directly inside their `select` --
// the wrapper has zero allocation cost beyond the existing make-channel
// inside Subscribe (the EventSubscription struct is stack-allocated by the
// caller's register-as-named-result path). Callers that need to thread the
// cancel callback into a defer chain or a peer-cleanup map continue to use
// the legacy Subscribe() return shape.
type EventSubscription struct {
	notify <-chan struct{}
	cancel func()
}

// Notify returns the channel that fires (non-blocking, buffered-1) on every
// EventLog.Append. Callers consume it inside a `select` arm. The channel is
// closed by Cancel() or by EventLog.CloseSubscribers when the underlying
// Process dies -- callers MUST NOT close it themselves.
func (s EventSubscription) Notify() <-chan struct{} { return s.notify }

// Cancel detaches this subscription from the EventLog and closes Notify().
// Idempotent: a second call is a no-op. Safe to call from any goroutine,
// including after CloseSubscribers has fired (the subscriber's closeOnce
// guard makes the close re-entry safe).
func (s EventSubscription) Cancel() {
	if s.cancel != nil {
		s.cancel()
	}
}

// SubscribeNew is the typed, package-encapsulated form of Subscribe.
// Returns an EventSubscription that owns both the notify channel and the
// cancel func, hiding (*subscriber).closeOnce semantics from cross-package
// callers per R246-ARCH-12 / #792. New call sites should prefer this entry
// point; Subscribe() is retained for the existing fleet of callers that
// already correctly wire the (channel, func) pair.
func (l *EventLog) SubscribeNew() EventSubscription {
	ch, cancel := l.Subscribe()
	return EventSubscription{notify: ch, cancel: cancel}
}

// Subscribe returns a notification channel and an unsubscribe function.
// The channel receives a signal (non-blocking) whenever Append is called.
//
// Prefer SubscribeNew for new code: it returns an EventSubscription value
// that bundles (channel, cancel) together so the channel-close contract
// stays an internal invariant of the eventlog package rather than a
// cross-package convention every caller must learn (R246-ARCH-12 / #792).
//
// If CloseSubscribers has already been called (process is dying), returns a
// channel that is already closed so the caller's select-on-notify arm fires
// immediately instead of parking forever. Without this guard, a Subscribe
// racing with readLoop's deferred CloseSubscribers would lazily rebuild the
// subscribers map and register a channel that nothing will ever close, so
// the downstream eventPushLoop would hang on <-notify until Hub shutdown.
func (l *EventLog) Subscribe() (<-chan struct{}, func()) {
	// R247-PERF-14 (#553): hot-check subsClosed BEFORE allocating the
	// subscriber struct + buffered channel. On dashboard reconnect storms
	// during shutdown a single dying EventLog can absorb hundreds of
	// late-arriving Subscribe attempts (each WS handshake registers a
	// session-tail subscriber); pre-sub-close the make(chan) + struct
	// literal would compose two heap allocations per call, only to be
	// immediately discarded after the closeOnce close + return-shared-
	// closed-channel hand-off below. The atomic counter is also load-only
	// so this short-circuit costs nothing on the steady-state happy path.
	//
	// Read l.subsClosed via a quick Lock/Unlock — atomic.Bool would suffice
	// but adding a third synchronization primitive next to subMu / subCount
	// is more failure-mode surface than warranted; the cold-path lock is
	// already taken on every Subscribe today. The shared eventLogClosedCh
	// singleton is a package-level pre-closed chan struct{} so all post-
	// close callers receive the same already-closed channel value rather
	// than a freshly-allocated-then-closed one. The closeOnce contract is
	// preserved by the no-op cancel func — callers MUST NOT close the
	// returned channel themselves (documented on Subscribe) and the shared
	// channel cannot be Cancelled twice into a double-close panic.
	l.subMu.Lock()
	if l.subsClosed {
		l.subMu.Unlock()
		return eventLogClosedCh, func() {}
	}
	sub := &subscriber{ch: make(chan struct{}, 1)}
	if l.subscribers == nil {
		// R230C-PERF-12 / R239-PERF-9: pre-size the slice. CloseSubscribers
		// nils out the slice so each Subscribe after a teardown allocates
		// a fresh backing array; without a cap hint Go would grow
		// 1 → 2 → 4 → 8 across a typical dashboard reconnect spurt (one
		// tab subscribes 4–6 sessions back-to-back). 4 covers the common
		// case in a single allocation; the slice still grows naturally
		// when the per-session subscriber count climbs (multi-tab
		// dashboards, agent_tailer fan-in).
		l.subscribers = make([]*subscriber, 0, 4)
	}
	l.subscribers = append(l.subscribers, sub)
	// Add/sub counter pattern rather than re-deriving from len(map) — avoids
	// the map-header read on each mutation and makes the reader/writer
	// asymmetry explicit (Load is on the hot notify path, Add is rare).
	// R65-PERF-L-4.
	l.subCount.Add(1)
	l.subMu.Unlock()

	unsub := func() {
		l.subMu.Lock()
		// Copy-on-write removal (R20260603000023-PERF-4 / #1647): build a
		// fresh backing array without `sub` instead of swap-to-end +
		// truncate in place. notifySubscribers snapshots the slice header
		// under a short RLock and reads it lock-free during the send loop;
		// an in-place mutation of the shared backing array would data-race
		// that snapshot. Allocating a new slice keeps every prior snapshot
		// immutable. Unsubscribe is the cold path (one per session/tab
		// lifetime), so the extra O(N) copy is off the hot notify path.
		for i, s := range l.subscribers {
			if s == sub {
				next := make([]*subscriber, 0, len(l.subscribers)-1)
				next = append(next, l.subscribers[:i]...)
				next = append(next, l.subscribers[i+1:]...)
				l.subscribers = next
				l.subCount.Add(-1)
				break
			}
		}
		l.subMu.Unlock()
		sub.close()
	}
	return sub.ch, unsub
}

// CloseSubscribers closes all subscriber channels and clears the subscriber list.
// Called when the process dies so that eventPushLoop goroutines can exit.
// After this returns, subsequent Subscribe calls receive a pre-closed channel.
func (l *EventLog) CloseSubscribers() {
	if l == nil {
		return
	}
	l.subMu.Lock()
	defer l.subMu.Unlock()
	for _, sub := range l.subscribers {
		sub.close()
	}
	l.subscribers = nil
	l.subCount.Store(0)
	l.subsClosed = true
}
