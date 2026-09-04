// File eventlog_subscribe.go: the subscriber broadcast leg — Subscribe(New),
// notifySubscribers, CloseSubscribers, and the EventSubscription handle
// (guarded by subMu, independent of the ring-buffer l.mu).

package cli

import "sync"

// subscriber is the per-Subscribe handle. Do not pool these: a closed channel
// cannot be reused (a recv would return the zero value forever) and sync.Once
// cannot be reset, so pooling would break the close-once invariant.
type subscriber struct {
	ch        chan struct{} // buffered(1)
	closeOnce sync.Once

	// mu guards the send-vs-close race on ch: signal() reads `closed` under
	// RLock, close() flips it and closes ch under Lock. This per-subscriber
	// lock is what lets notifySubscribers release subMu before its send loop
	// (#1647), so concurrent notify waves do not serialise on subMu.
	mu     sync.RWMutex
	closed bool
}

// signal performs the non-blocking wake send, never sending on a closed channel.
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

// close flips `closed` and closes ch under the write lock, exactly once;
// mutually exclusive with signal() so no send can race the close.
func (s *subscriber) close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.ch)
		s.mu.Unlock()
	})
}

// eventLogClosedCh is a shared pre-closed channel returned by Subscribe after
// CloseSubscribers, so late subscribers during reconnect storms allocate
// nothing. It has no senders, so sharing is permanently safe; a recv returns
// ok=false exactly like a freshly closed channel (#553).
var eventLogClosedCh = func() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// notifySubscribers wakes all subscriber channels non-blockingly.
//
// subMu.RLock is held only to snapshot the slice header and released BEFORE
// the send loop; close-vs-send safety lives on each subscriber's own mu
// (#1647). A snapshot may include a subscriber that unsub removes mid-loop —
// signal() on it is harmless (ignored wake or no-op after close). Idle
// sessions skip subMu entirely via the atomic subCount.
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

// EventSubscription bundles an EventLog notification channel with its cancel
// func so cross-package consumers do not have to learn the raw channel's
// close contract (closed exactly once by Cancel or CloseSubscribers; never by
// the caller) (#792). Hot-path callers still select on Notify() directly.
type EventSubscription struct {
	notify <-chan struct{}
	cancel func()
}

// Notify returns the channel that fires (non-blocking, buffered-1) on every
// EventLog.Append. It is closed by Cancel() or EventLog.CloseSubscribers when
// the Process dies — callers MUST NOT close it themselves.
func (s EventSubscription) Notify() <-chan struct{} { return s.notify }

// Cancel detaches this subscription and closes Notify(). Idempotent and safe
// from any goroutine, including after CloseSubscribers has fired.
func (s EventSubscription) Cancel() {
	if s.cancel != nil {
		s.cancel()
	}
}

// SubscribeNew is the typed form of Subscribe; prefer it for new call sites.
func (l *EventLog) SubscribeNew() EventSubscription {
	ch, cancel := l.Subscribe()
	return EventSubscription{notify: ch, cancel: cancel}
}

// Subscribe returns a notification channel and an unsubscribe function.
// The channel receives a signal (non-blocking) whenever Append is called.
//
// If CloseSubscribers has already run (process dying) the returned channel is
// already closed so the caller's select arm fires instead of parking forever;
// otherwise a Subscribe racing readLoop's deferred CloseSubscribers would
// register a channel nothing ever closes and hang eventPushLoop.
func (l *EventLog) Subscribe() (<-chan struct{}, func()) {
	// Check subsClosed BEFORE allocating: a dying EventLog can absorb hundreds
	// of late Subscribe attempts during a reconnect storm (#553). The shared
	// pre-closed channel plus no-op cancel keeps the close-once contract.
	l.subMu.Lock()
	if l.subsClosed {
		l.subMu.Unlock()
		return eventLogClosedCh, func() {}
	}
	sub := &subscriber{ch: make(chan struct{}, 1)}
	if l.subscribers == nil {
		// cap 4 covers a typical reconnect spurt (one tab subscribing 4-6
		// sessions) in one allocation after CloseSubscribers nils the slice.
		l.subscribers = make([]*subscriber, 0, 4)
	}
	l.subscribers = append(l.subscribers, sub)
	l.subCount.Add(1)
	l.subMu.Unlock()

	unsub := func() {
		l.subMu.Lock()
		// Copy-on-write removal: notifySubscribers reads its slice snapshot
		// lock-free, so the shared backing array must never be mutated in
		// place. Unsubscribe is cold, the O(N) copy is fine (#1647).
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
