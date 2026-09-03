package platform

import (
	"log/slog"
	"runtime/debug"
	"sync"
)

// DefaultHandlerConcurrency caps concurrent inbound message-handler
// goroutines per platform adapter. It is the single shared constant behind
// every adapter's BoundedDispatch (feishu / slack / discord / weixin); the
// per-package copies that used to "mirror feishu.hookSem (20)" by comment
// alone were removed in #2254. Raise per-deployment via config only after
// observing handler queue depth in production.
const DefaultHandlerConcurrency = 20

// BoundedDispatch is the inbound-handler skeleton shared by every platform
// adapter: a non-blocking semaphore bounding concurrent handlers, a WaitGroup
// so Stop() can drain in-flight work, panic recovery per handler, and a
// uniform slog.Warn when a message is dropped because the semaphore is full.
//
// Before #2254 each adapter re-derived this by hand (hookSem + handlerWg +
// RecoverHandler + drop-warn) and #1947 / #2009 showed how easily a single
// adapter drifts. Adapters now hold one BoundedDispatch value and call
// TryGo for bounded handler goroutines, Go for tracked-but-unbounded
// background work (bot-identity self-heal), TryRun for bounded work that
// must stay on the caller's goroutine, and Wait from Stop().
//
// Ordering invariant (R184-CONCUR-H1): wg.Add(1) always runs on the caller's
// goroutine before `go`, so a concurrent Wait() can never observe count 0
// between dispatch and Add. Callers must not submit after Wait() has been
// called from a terminal Stop() — the same WaitGroup contract the hand-rolled
// skeletons had.
//
// The zero value is ready to use (Name "" logs as "platform", Cap 0 means
// DefaultHandlerConcurrency), so adapters built as bare struct literals in
// tests keep working. Must not be copied after first use.
type BoundedDispatch struct {
	// Name prefixes the drop warning, e.g. "slack: handler semaphore full,
	// dropping message". Set once at construction.
	Name string
	// Cap is the semaphore size; 0 selects DefaultHandlerConcurrency.
	// Production adapters leave it 0 so all platforms share one cap.
	Cap int

	once sync.Once
	sem  chan struct{}
	wg   sync.WaitGroup
}

func (b *BoundedDispatch) init() {
	b.once.Do(func() {
		c := b.Cap
		if c <= 0 {
			c = DefaultHandlerConcurrency
		}
		b.sem = make(chan struct{}, c)
	})
}

func (b *BoundedDispatch) logName() string {
	if b.Name == "" {
		return "platform"
	}
	return b.Name
}

// TryAcquire takes a semaphore slot without blocking. Returns false when the
// pool is saturated; the caller decides how to report the drop (TryGo/TryRun
// do this for you). Every true must be paired with exactly one Release.
func (b *BoundedDispatch) TryAcquire() bool {
	b.init()
	select {
	case b.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release returns a slot taken by a successful TryAcquire.
func (b *BoundedDispatch) Release() {
	<-b.sem
}

func (b *BoundedDispatch) warnDrop(label string, attrs []any) {
	args := make([]any, 0, 2+len(attrs))
	args = append(args, "handler", label)
	args = append(args, attrs...)
	slog.Warn(b.logName()+": handler semaphore full, dropping message", args...)
}

// TryGo runs fn on a new goroutine if a semaphore slot is free: the goroutine
// is tracked by Wait(), releases its slot on exit, and recovers panics under
// label. When saturated it logs a Warn carrying label plus attrs and returns
// false without running fn — drop, never block, so a flood cannot spawn
// unbounded goroutines or stall the platform SDK's read loop.
func (b *BoundedDispatch) TryGo(label string, fn func(), attrs ...any) bool {
	if !b.TryAcquire() {
		b.warnDrop(label, attrs)
		return false
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.Release()
		defer recoverHandler(label)
		fn()
	}()
	return true
}

// TryRun is TryGo without the goroutine: fn runs synchronously on the caller
// under the same slot + Wait tracking + panic recovery. For SDK callbacks
// that must complete before returning a response (feishu ws card action).
// Returns true whenever fn was entered — including when fn panicked and was
// recovered — and false only for a drop.
func (b *BoundedDispatch) TryRun(label string, fn func(), attrs ...any) (ran bool) {
	if !b.TryAcquire() {
		b.warnDrop(label, attrs)
		return false
	}
	b.wg.Add(1)
	defer b.wg.Done()
	defer b.Release()
	defer recoverHandler(label)
	ran = true // set before fn so a recovered panic still reports "ran"
	fn()
	return ran
}

// Go runs fn on a new goroutine tracked by Wait() with panic recovery, but
// without taking a semaphore slot. For rate-limited background work that
// must not compete with inbound handlers for slots yet must still be drained
// by Stop() (bot-identity self-heal).
func (b *BoundedDispatch) Go(label string, fn func()) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer recoverHandler(label)
		fn()
	}()
}

// Wait blocks until every goroutine started by TryGo/Go and every in-flight
// TryRun has returned. Call from Stop() after the inbound source is closed.
func (b *BoundedDispatch) Wait() {
	b.wg.Wait()
}

// recoverHandler catches panics in platform message handler goroutines,
// preventing a single malformed message from crashing the entire platform
// listener. Unexported since #2254: adapters get it through BoundedDispatch
// rather than hand-rolling the defer chain.
func recoverHandler(label string) {
	if r := recover(); r != nil {
		slog.Error("panic in platform handler (recovered)",
			"handler", label, "panic", r, "stack", string(debug.Stack()))
	}
}
