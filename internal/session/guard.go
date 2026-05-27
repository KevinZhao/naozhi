package session

import (
	"context"
	"sync"
	"time"
)

// Guard prevents multiple concurrent messages to the same session.
type Guard struct {
	active   sync.Map             // string → struct{}: sessions currently processing a message
	cond     *sync.Cond           // broadcast on Release to wake all AcquireTimeout waiters
	waitMu   sync.Mutex           // guards lastWait
	lastWait map[string]time.Time // tracks last "please wait" reply per key
}

// NewGuard creates a new Guard.
func NewGuard() *Guard {
	g := &Guard{
		lastWait: make(map[string]time.Time),
	}
	g.cond = sync.NewCond(&sync.Mutex{})
	return g
}

// TryAcquire attempts to acquire the guard for key. Returns true if successful.
func (g *Guard) TryAcquire(key string) bool {
	_, loaded := g.active.LoadOrStore(key, struct{}{})
	return !loaded
}

// waitReplyDedupeWindow caps how often the dispatcher will reply with a
// "please wait" notice for the same session key. Bursts of user messages
// while the CLI is still processing one turn would otherwise spam the IM
// channel; the window collapses N rapid attempts into a single notice.
const waitReplyDedupeWindow = 3 * time.Second

// lastWaitPruneThreshold is the lastWait map size at which ShouldSendWait
// runs a single opportunistic sweep to drop entries older than the
// dedupe-relevance horizon. Keeps the map bounded for paths that hit
// ShouldSendWait without ever reaching Release (R217-GO-1 / #1306).
const lastWaitPruneThreshold = 256

// lastWaitStale is how old a lastWait entry must be before opportunistic
// prune drops it. Anything past 10 × dedupe-window is stale w.r.t. a fresh
// "please wait" decision; the timestamp would already let ShouldSendWait
// fire the notice on the next call for that key.
const lastWaitStale = 10 * waitReplyDedupeWindow

// ShouldSendWait returns true if enough time has passed since the last
// "please wait" reply for this key (avoids spamming the user).
//
// R217-GO-1 (#1306): paths that never call Release would otherwise grow
// lastWait unbounded (one entry per unique key seen busy). When the map
// crosses lastWaitPruneThreshold this method opportunistically drops
// entries older than lastWaitStale before adding the new one — keeps
// the map self-bounded without a dedicated sweeper goroutine. The walk
// is linear in map size but only runs once size exceeds the threshold,
// amortising to O(1) per call across normal load.
func (g *Guard) ShouldSendWait(key string) bool {
	g.waitMu.Lock()
	defer g.waitMu.Unlock()
	if time.Since(g.lastWait[key]) < waitReplyDedupeWindow {
		return false
	}
	now := time.Now()
	if len(g.lastWait) >= lastWaitPruneThreshold {
		// One-shot O(N) sweep. The threshold keeps the amortised cost low;
		// `cutoff` uses Add(-) so a backwards NTP step (now < entry ts)
		// produces a future cutoff that fails the .Before comparison and
		// leaves the entry alone — a fresh notice on its next call still
		// short-circuits via the dedupe-window check above.
		cutoff := now.Add(-lastWaitStale)
		for k, ts := range g.lastWait {
			if ts.Before(cutoff) {
				delete(g.lastWait, k)
			}
		}
	}
	g.lastWait[key] = now
	return true
}

// Release releases the guard for key.
func (g *Guard) Release(key string) {
	g.active.Delete(key)
	g.waitMu.Lock()
	delete(g.lastWait, key)
	g.waitMu.Unlock()
	// Wake all AcquireTimeout waiters so they can re-check their key.
	g.cond.L.Lock()
	g.cond.Broadcast()
	g.cond.L.Unlock()
}

// AcquireTimeout tries to acquire the guard, waiting for Release notification,
// context cancellation, or timeout — whichever comes first.
func (g *Guard) AcquireTimeout(ctx context.Context, key string, timeout time.Duration) bool {
	if g.TryAcquire(key) {
		return true
	}
	deadline := time.Now().Add(timeout)
	var closeOnce sync.Once
	done := make(chan struct{})
	closeDone := func() { closeOnce.Do(func() { close(done) }) }
	timer := time.AfterFunc(timeout, func() {
		closeDone()
		// Hold cond.L while Broadcasting to avoid the missed-wakeup race
		// with Wait's recheck loop (see sync.Cond docs).
		g.cond.L.Lock()
		g.cond.Broadcast()
		g.cond.L.Unlock()
	})
	localDone := make(chan struct{})
	defer close(localDone)
	// Also broadcast on context cancellation to unblock Wait promptly.
	// Skip the goroutine entirely when ctx is non-cancellable (e.g.
	// context.Background or a context.WithoutCancel-derived ctx with a
	// nil Done channel) — receiving from a nil channel blocks forever
	// and the wakeup arm is structurally unreachable.
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				g.cond.L.Lock()
				g.cond.Broadcast()
				g.cond.L.Unlock()
			case <-localDone:
			}
		}()
	}
	g.cond.L.Lock()
	defer func() {
		g.cond.L.Unlock()
		timer.Stop()
		closeDone()
	}()
	for {
		if g.TryAcquire(key) {
			return true
		}
		select {
		case <-done:
			return false
		case <-ctx.Done():
			return false
		default:
		}
		if time.Now().After(deadline) {
			return false
		}
		g.cond.Wait()
	}
}
