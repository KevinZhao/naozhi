package platform

import (
	"log/slog"
	"runtime/debug"
	"sync"
)

// DefaultHandlerConcurrency caps concurrent inbound handler goroutines per
// platform adapter — the single constant behind every BoundedDispatch (#2254).
const DefaultHandlerConcurrency = 20

// BoundedDispatch is the inbound-handler skeleton shared by every adapter: a
// non-blocking semaphore, a WaitGroup so Stop() can drain, per-handler panic
// recovery and a uniform drop warning. TryGo = bounded goroutine, Go =
// tracked-but-unbounded background work, TryRun = bounded work on the
// caller's goroutine, Wait from Stop().
//
// Invariant: wg.Add(1) runs on the caller's goroutine before `go`, so a
// concurrent Wait() never observes count 0 mid-dispatch; callers must not
// submit after a terminal Stop() has called Wait(). Zero value is ready
// (Name "" logs as "platform", Cap 0 = default). Do not copy after first use.
type BoundedDispatch struct {
	// Name prefixes the drop warning ("slack: handler semaphore full, ...").
	Name string
	// Cap is the semaphore size; 0 selects DefaultHandlerConcurrency.
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

// TryAcquire takes a slot without blocking; false when saturated. Every true
// must be paired with exactly one Release.
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

// TryGo runs fn on a tracked goroutine if a slot is free; when saturated it
// warns and returns false without running fn — drop, never block, so a flood
// cannot stall the platform SDK's read loop.
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

// TryRun is TryGo without the goroutine, for SDK callbacks that must complete
// before returning. True whenever fn was entered (even if it panicked), false
// only for a drop.
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

// Go runs fn on a tracked goroutine without taking a slot — for rate-limited
// background work (bot-identity self-heal) that Stop() must still drain.
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

// recoverHandler keeps one malformed message from crashing the platform listener.
func recoverHandler(label string) {
	if r := recover(); r != nil {
		slog.Error("panic in platform handler (recovered)",
			"handler", label, "panic", r, "stack", string(debug.Stack()))
	}
}
