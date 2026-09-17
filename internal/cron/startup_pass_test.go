package cron

import (
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureSlog redirects the default logger into a buffer for the duration of the
// test. Returns a reader because the pass logs from its own goroutine.
func captureSlog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf strings.Builder
	handler := slog.NewTextHandler(&lockedWriter{mu: &mu, b: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})
	orig := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// TestGoStartupPass_PanicDoesNotEscape is the defect: all three cold-start passes
// ran bare, so a panic in any of them took the process down before the dashboard
// was served — and because the next boot reads the same on-disk state, that is a
// crash loop with no bound of its own.
//
// A panic escaping a goroutine cannot be caught by the test either, so "the test
// still runs to completion" IS the assertion; a regression here fails the whole
// package run rather than this one case.
func TestGoStartupPass_PanicDoesNotEscape(t *testing.T) {
	s := &Scheduler{}
	logs := captureSlog(t)

	s.goStartupPass("boom-pass", func() { panic("pass exploded") })

	// Stop's contract: gcWG is released even for a pass that died. Without the
	// recover the process is already gone; with the Done in the wrong place this
	// hangs.
	waitWithTimeout(t, &s.gcWG, 5*time.Second)

	out := logs()
	if !strings.Contains(out, "boom-pass") {
		t.Errorf("log does not name the pass; an operator cannot tell which reconciliation was skipped.\ngot: %s", out)
	}
	if !strings.Contains(out, "pass exploded") {
		t.Errorf("log does not carry the panic value.\ngot: %s", out)
	}
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("a skipped cold-start pass must log at ERROR, not below.\ngot: %s", out)
	}
}

// TestGoStartupPass_RunsTheFunctionAndIsWaitable pins the other half: the pass
// really runs, and Stop can wait for it. A recover wrapper that swallowed the
// call would satisfy the panic test alone.
func TestGoStartupPass_RunsTheFunctionAndIsWaitable(t *testing.T) {
	s := &Scheduler{}
	done := make(chan struct{})
	s.goStartupPass("ok-pass", func() { close(done) })

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pass function never ran")
	}
	waitWithTimeout(t, &s.gcWG, 5*time.Second)
}

// TestGoStartupPass_CountsBeforeTheGoroutineStarts: gcWG.Add must happen on the
// caller's goroutine. If it moved inside, a Stop arriving between Start's return
// and the pass's first instruction would Wait on a zero counter and return while
// the pass is still touching the run store.
func TestGoStartupPass_CountsBeforeTheGoroutineStarts(t *testing.T) {
	s := &Scheduler{}
	release := make(chan struct{})
	s.goStartupPass("slow-pass", func() { <-release })

	// The pass is blocked, so the counter must be held right now — Wait has to
	// block. Racing this with a short timer is the only way to observe "Wait did
	// not return immediately"; a Wait that returns here means the pass was not
	// counted when goStartupPass returned.
	waitReturned := make(chan struct{})
	go func() {
		s.gcWG.Wait()
		close(waitReturned)
	}()
	select {
	case <-waitReturned:
		close(release)
		t.Fatal("gcWG.Wait returned while the pass was still running; Stop would not wait for it")
	case <-time.After(150 * time.Millisecond):
	}

	close(release)
	waitWithTimeout(t, &s.gcWG, 5*time.Second)
}

func waitWithTimeout(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("gcWG.Wait did not return; a cold-start pass leaked its counter and Stop would hang")
	}
}
