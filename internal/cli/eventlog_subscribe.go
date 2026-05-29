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
// Holds subMu as a reader for the full iteration: CloseSubscribers takes the
// write lock and uses sub.closeOnce to ensure each channel is closed exactly
// once. The send-on-closed-chan race is avoided by the RWMutex rather than
// by the channel send itself — Go's channel-send-is-goroutine-safe guarantee
// does NOT extend to sending on a closed channel, which panics. Multiple
// concurrent notifySubscribers readers are safe to iterate and signal the
// same channel set because non-blocking sends on an open channel are allowed
// to race.
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
// RNEW-PERF-004 (#455) — DO-NOT-DO note: a tempting micro-optimisation
// is to snapshot `l.subscribers` under RLock then drop the lock before
// the per-channel send loop, on the theory that the non-blocking sends
// no longer need lock protection. This is UNSAFE in the current design:
// the unsub closure (Subscribe's returned cancel func) closes
// `sub.ch` *outside* l.subMu.Lock — `closeOnce.Do(func() { close(sub.ch) })`
// runs after the lock release at line ~1556 below. With a snapshot-
// then-unlock notify, the following sequence panics:
//
//	notify: RLock; copy l.subscribers into local; RUnlock
//	unsub:  Lock; remove sub from slice; Unlock; close(sub.ch)
//	notify: select { case sub.ch <- struct{}{}: …  ← PANIC: send on closed
//
// Today's RLock-around-send blocks unsub.Lock until the iteration ends,
// so close(sub.ch) cannot run while a notify still holds a reference to
// the channel. Any future "snapshot then unlock" attempt MUST first move
// the close into the lock AND ensure no in-flight notify can hold a
// reference to a *subscriber that has been removed from l.subscribers
// (e.g. via subscriber refcount drained under Lock, or by switching
// from close(ch) to a separate atomic "dead" flag observed before send).
// The current cost — RLock acquire/release per Append — is bounded by
// the subCount==0 fast path: idle sessions skip subMu entirely, and the
// reader-only RWMutex makes concurrent Appends across different sessions
// fan out without serialising on a single Mutex. Issue #455 is filed at
// MEDIUM/defense-in-depth and remains accepted as-is until the close-
// timing rework lands.
func (l *EventLog) notifySubscribers() {
	if l.subCount.Load() == 0 {
		return
	}
	l.subMu.RLock()
	for _, sub := range l.subscribers {
		select {
		case sub.ch <- struct{}{}:
		default:
		}
	}
	l.subMu.RUnlock()
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
		// Linear scan + swap-to-end + truncate. Subscribers count is
		// typically 1-10 (one per dashboard tab subscribed to this
		// session), so this is O(N) on the cold unsubscribe path. The
		// hot notifySubscribers path benefits in exchange. R239-PERF-9.
		for i, s := range l.subscribers {
			if s == sub {
				last := len(l.subscribers) - 1
				l.subscribers[i] = l.subscribers[last]
				// Clear the trailing slot so the removed *subscriber
				// is not retained by the backing array; otherwise a
				// long-lived EventLog holds onto closed subscriber
				// objects past their useful life.
				l.subscribers[last] = nil
				l.subscribers = l.subscribers[:last]
				l.subCount.Add(-1)
				break
			}
		}
		l.subMu.Unlock()
		sub.closeOnce.Do(func() { close(sub.ch) })
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
		sub.closeOnce.Do(func() { close(sub.ch) })
	}
	l.subscribers = nil
	l.subCount.Store(0)
	l.subsClosed = true
}
