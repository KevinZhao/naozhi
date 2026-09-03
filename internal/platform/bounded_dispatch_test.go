package platform

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// captureLogs swaps slog's default handler for a buffer at Warn level and
// returns a reader. Not parallel-safe: tests using it must not t.Parallel().
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// waitReturns reports whether b.Wait() returns within d.
func waitReturns(b *BoundedDispatch, d time.Duration) bool {
	done := make(chan struct{})
	go func() { b.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestBoundedDispatch_ZeroValueUsesDefaultCap pins that a bare struct literal
// (how many adapter tests build their fixtures) is a working pool sized at
// DefaultHandlerConcurrency, and that Name "" still logs a usable prefix.
func TestBoundedDispatch_ZeroValueUsesDefaultCap(t *testing.T) {
	logs := captureLogs(t)
	var b BoundedDispatch
	n := 0
	for b.TryAcquire() {
		n++
		if n > DefaultHandlerConcurrency {
			t.Fatalf("acquired %d slots, cap should be %d", n, DefaultHandlerConcurrency)
		}
	}
	if n != DefaultHandlerConcurrency {
		t.Fatalf("zero-value cap = %d, want %d", n, DefaultHandlerConcurrency)
	}
	if b.TryGo("zero", func() { t.Error("fn must not run when saturated") }) {
		t.Fatal("TryGo returned true on a saturated pool")
	}
	if out := logs(); !strings.Contains(out, "platform: handler semaphore full, dropping message") || !strings.Contains(out, "handler=zero") {
		t.Fatalf("drop warning missing or wrong prefix: %q", out)
	}
	for i := 0; i < n; i++ {
		b.Release()
	}
	if !b.TryAcquire() {
		t.Fatal("slot not free after Release")
	}
	b.Release()
}

// TestBoundedDispatch_TryGoDropsWhenSaturated fills a Cap=2 pool with two
// blocked handlers and asserts the third submission is dropped with a Warn
// carrying the label and caller attrs, never blocking and never running fn.
func TestBoundedDispatch_TryGoDropsWhenSaturated(t *testing.T) {
	logs := captureLogs(t)
	b := &BoundedDispatch{Name: "unit", Cap: 2}
	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(2)
	for i := 0; i < 2; i++ {
		if !b.TryGo("unit", func() { started.Done(); <-release }) {
			t.Fatalf("TryGo #%d rejected below cap", i)
		}
	}
	started.Wait()

	ran := make(chan struct{}, 1)
	returned := make(chan bool, 1)
	go func() { returned <- b.TryGo("unit", func() { ran <- struct{}{} }, "chat", "c1", "user", "u1") }()
	select {
	case ok := <-returned:
		if ok {
			t.Fatal("TryGo returned true on a saturated pool")
		}
	case <-time.After(time.Second):
		t.Fatal("TryGo blocked on a saturated pool; must be non-blocking")
	}
	select {
	case <-ran:
		t.Fatal("dropped fn must not run")
	case <-time.After(50 * time.Millisecond):
	}
	out := logs()
	for _, want := range []string{"unit: handler semaphore full, dropping message", "handler=unit", "chat=c1", "user=u1"} {
		if !strings.Contains(out, want) {
			t.Errorf("drop warning missing %q in %q", want, out)
		}
	}

	if waitReturns(b, 50*time.Millisecond) {
		t.Fatal("Wait returned while two handlers were still in flight")
	}
	close(release)
	if !waitReturns(b, 2*time.Second) {
		t.Fatal("Wait did not return after handlers finished")
	}
	// Both slots must be back.
	if !b.TryAcquire() || !b.TryAcquire() {
		t.Fatal("slots not released after handlers exited")
	}
	if b.TryAcquire() {
		t.Fatal("acquired a third slot on a Cap=2 pool")
	}
	b.Release()
	b.Release()
}

// TestBoundedDispatch_PanicRecoveredReleasesSlot: a panicking handler must
// not crash the process, must release its slot, must Done its wg count, and
// must not disturb a concurrently running healthy handler.
func TestBoundedDispatch_PanicRecoveredReleasesSlot(t *testing.T) {
	logs := captureLogs(t)
	b := &BoundedDispatch{Name: "unit", Cap: 2}
	release := make(chan struct{})
	var healthyDone atomic.Bool
	if !b.TryGo("healthy", func() { <-release; healthyDone.Store(true) }) {
		t.Fatal("healthy TryGo rejected")
	}
	if !b.TryGo("boom", func() { panic("kaboom") }) {
		t.Fatal("panicking TryGo rejected")
	}
	// The panicking goroutine must exit and free its slot even though the
	// healthy one is still parked; poll for the freed slot.
	deadline := time.Now().Add(2 * time.Second)
	for !b.TryAcquire() {
		if time.Now().After(deadline) {
			t.Fatal("slot held by panicking handler was never released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	b.Release()
	out := logs()
	if !strings.Contains(out, "panic in platform handler (recovered)") || !strings.Contains(out, "handler=boom") || !strings.Contains(out, "kaboom") {
		t.Fatalf("panic not logged with label: %q", out)
	}
	if healthyDone.Load() {
		t.Fatal("healthy handler finished early; test setup broken")
	}
	close(release)
	if !waitReturns(b, 2*time.Second) {
		t.Fatal("Wait did not return; panicking handler leaked a wg count")
	}
	if !healthyDone.Load() {
		t.Fatal("healthy handler did not complete")
	}
}

// TestBoundedDispatch_TryRunSynchronous pins the inline variant: fn runs on
// the caller before TryRun returns, the slot is released afterwards, panics
// are recovered, and a saturated pool drops without running fn.
func TestBoundedDispatch_TryRunSynchronous(t *testing.T) {
	logs := captureLogs(t)
	b := &BoundedDispatch{Name: "unit", Cap: 1}
	ran := false
	if !b.TryRun("inline", func() { ran = true }) {
		t.Fatal("TryRun rejected on an empty pool")
	}
	if !ran {
		t.Fatal("TryRun returned before fn ran")
	}
	if !b.TryRun("inline-panic", func() { panic("inline boom") }) {
		t.Fatal("TryRun with panicking fn should still report it ran")
	}
	if !strings.Contains(logs(), "handler=inline-panic") {
		t.Fatalf("inline panic not recovered/logged: %q", logs())
	}
	if !b.TryAcquire() {
		t.Fatal("TryRun leaked its slot")
	}
	dropped := b.TryRun("inline", func() { t.Error("fn must not run when saturated") }, "k", "v")
	if dropped {
		t.Fatal("TryRun returned true on a saturated pool")
	}
	if !strings.Contains(logs(), "handler=inline k=v") {
		t.Fatalf("TryRun drop warning missing attrs: %q", logs())
	}
	b.Release()
	if !waitReturns(b, time.Second) {
		t.Fatal("Wait blocked after synchronous runs completed")
	}
}

// TestBoundedDispatch_GoTrackedWithoutSlot: Go must not consume a semaphore
// slot (so self-heal never competes with inbound handlers), must be drained
// by Wait, and must recover panics.
func TestBoundedDispatch_GoTrackedWithoutSlot(t *testing.T) {
	logs := captureLogs(t)
	b := &BoundedDispatch{Name: "unit", Cap: 1}
	if !b.TryAcquire() {
		t.Fatal("initial acquire failed")
	}
	release := make(chan struct{})
	b.Go("bg", func() { <-release })
	b.Go("bg-panic", func() { panic("bg boom") })
	if waitReturns(b, 50*time.Millisecond) {
		t.Fatal("Wait returned while Go goroutine in flight")
	}
	close(release)
	if !waitReturns(b, 2*time.Second) {
		t.Fatal("Wait did not drain Go goroutines")
	}
	if !strings.Contains(logs(), "handler=bg-panic") {
		t.Fatalf("Go panic not recovered/logged: %q", logs())
	}
	b.Release()
}

// TestBoundedDispatch_ConcurrentNeverExceedsCap hammers TryGo from many
// goroutines under -race and asserts the observed in-flight high-water mark
// never exceeds Cap, every submission is either run or dropped exactly once,
// and Wait drains everything.
func TestBoundedDispatch_ConcurrentNeverExceedsCap(t *testing.T) {
	captureLogs(t) // silence drop warnings
	const capacity = 4
	const submissions = 200
	b := &BoundedDispatch{Name: "unit", Cap: capacity}
	var inFlight, high, ran, dropped atomic.Int32
	var submitters sync.WaitGroup
	for i := 0; i < submissions; i++ {
		submitters.Add(1)
		go func() {
			defer submitters.Done()
			ok := b.TryGo("stress", func() {
				cur := inFlight.Add(1)
				for {
					h := high.Load()
					if cur <= h || high.CompareAndSwap(h, cur) {
						break
					}
				}
				time.Sleep(200 * time.Microsecond)
				inFlight.Add(-1)
				ran.Add(1)
			})
			if !ok {
				dropped.Add(1)
			}
		}()
	}
	submitters.Wait()
	if !waitReturns(b, 5*time.Second) {
		t.Fatal("Wait did not drain")
	}
	if got := ran.Load() + dropped.Load(); got != submissions {
		t.Fatalf("ran+dropped = %d, want %d", got, submissions)
	}
	if ran.Load() == 0 {
		t.Fatal("no handler ran")
	}
	if high.Load() > capacity {
		t.Fatalf("in-flight high-water %d exceeded cap %d", high.Load(), capacity)
	}
	if inFlight.Load() != 0 {
		t.Fatalf("in-flight %d after Wait", inFlight.Load())
	}
	for i := 0; i < capacity; i++ {
		if !b.TryAcquire() {
			t.Fatalf("slot %d not returned after drain", i)
		}
	}
}
