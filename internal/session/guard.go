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

// lastWaitSweepThreshold triggers an opportunistic O(N) cleanup of the
// lastWait map once it crosses this size. Entries are normally pruned by
// Release, but TryAcquire-then-bail paths (busy queues that never reach
// Release) can leak entries. The threshold is tuned for single-operator
// deployments where steady-state chat count is well under 256; bursts
// above that trigger the sweep, amortising to O(1) per ShouldSendWait
// call under sustained load.
const lastWaitSweepThreshold = 256

// lastWaitStaleMultiplier defines stale-entry age relative to the dedupe
// window. 10× gives us a comfortable margin: any entry older than this
// can no longer affect ShouldSendWait's return value, so it is safe to
// drop without changing observable behaviour.
const lastWaitStaleMultiplier = 10

// ShouldSendWait returns true if enough time has passed since the last
// "please wait" reply for this key (avoids spamming the user).
func (g *Guard) ShouldSendWait(key string) bool {
	g.waitMu.Lock()
	defer g.waitMu.Unlock()
	if time.Since(g.lastWait[key]) < waitReplyDedupeWindow {
		return false
	}
	g.lastWait[key] = time.Now()
	// Opportunistic sweep: TryAcquire-then-bail paths leak entries because
	// only Release deletes them. Once the map exceeds the threshold, drop
	// entries older than 10× the dedupe window — they cannot affect future
	// ShouldSendWait decisions and amortise the cost across busy bursts.
	if len(g.lastWait) > lastWaitSweepThreshold {
		cutoff := time.Now().Add(-lastWaitStaleMultiplier * waitReplyDedupeWindow)
		for k, ts := range g.lastWait {
			if ts.Before(cutoff) {
				delete(g.lastWait, k)
			}
		}
	}
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
