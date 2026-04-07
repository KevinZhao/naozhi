package session

import (
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

// ShouldSendWait returns true if enough time has passed since the last
// "please wait" reply for this key (avoids spamming the user).
func (g *Guard) ShouldSendWait(key string) bool {
	g.waitMu.Lock()
	defer g.waitMu.Unlock()
	if time.Since(g.lastWait[key]) < 3*time.Second {
		return false
	}
	g.lastWait[key] = time.Now()
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

// AcquireTimeout tries to acquire the guard, waiting for Release notification or timeout.
func (g *Guard) AcquireTimeout(key string, timeout time.Duration) bool {
	if g.TryAcquire(key) {
		return true
	}
	deadline := time.Now().Add(timeout)
	done := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
		close(done)
		g.cond.Broadcast() // unblock Wait
	})
	g.cond.L.Lock()
	defer func() {
		g.cond.L.Unlock()
		timer.Stop()
	}()
	for {
		if g.TryAcquire(key) {
			return true
		}
		select {
		case <-done:
			return false
		default:
		}
		if time.Now().After(deadline) {
			return false
		}
		g.cond.Wait()
	}
}
