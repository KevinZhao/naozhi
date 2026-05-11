package session

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// RNEW-006 — the idle branch of ManagedSession.Interrupt() (proc == nil
// || !proc.Alive()) must still cancel an in-flight Send() context. The
// original bug: the dead-or-idle branch returned `false` without loading
// sendCancel, so a Send() blocked on ctx.Done() would wait forever if its
// process was swapped out (dead, replaced, not yet stored) between Send()
// storing sendCancel and Interrupt() reading loadProcess().
//
// The fix lives at internal/session/managed.go:580-594 (commit f50c1142):
// both the dead-branch early return AND the alive-branch tail call
// sendCancel.Load() + invoke it when non-nil. These two tests pin the
// behaviour under -race and at the source level so a future refactor
// cannot silently reopen the race.

// TestInterrupt_IdleBranch_CancelsInFlightSend exercises the exact race
// shape RNEW-006 described: Send() is blocked waiting on its ctx while the
// live process appears not-alive to a concurrent Interrupt(). Without the
// sendCancel.Load() in the idle branch, Send()'s ctx is never cancelled and
// this test deadlocks until the outer timeout, which we treat as failure.
func TestInterrupt_IdleBranch_CancelsInFlightSend(t *testing.T) {
	t.Parallel()

	proc := NewTestProcess()
	// Pin the process to the dead/idle branch BEFORE any goroutine starts
	// — flipping AliveVal later would race Interrupt's proc.Alive() read
	// (AliveVal is a plain bool on the test stub, not atomic) AND let the
	// test pass via the wrong branch on unlucky scheduling. Static
	// pre-set guarantees Interrupt takes the RNEW-006 idle path every
	// invocation.
	proc.AliveVal = false
	// Gate the Send() call so we can assert ordering: Send must store
	// sendCancel BEFORE Interrupt runs. The SendFunc blocks on ctx.Done().
	sendEntered := make(chan struct{})
	proc.SendFunc = func(ctx context.Context, _ string, _ []cli.ImageData, _ cli.EventCallback) (*cli.SendResult, error) {
		close(sendEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	s := &ManagedSession{key: "rnew006-idle"}
	s.storeProcess(proc)
	s.touchLastActive()

	sendErr := make(chan error, 1)
	go func() {
		_, err := s.Send(context.Background(), "blocking-send", nil, nil)
		sendErr <- err
	}()

	// Wait for Send() to enter the user function so sendCancel.Store has
	// completed. Without this handshake the race could still be "won" by
	// Interrupt() loading sendCancel == nil.
	select {
	case <-sendEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Send() never entered SendFunc")
	}

	// Fire Interrupt. proc.Alive() returns false (pre-set above), so
	// Interrupt falls into the idle branch. Pre-fix this would return
	// false without cancelling and the Send goroutine would hang forever.
	if got := s.Interrupt(); got != false {
		t.Errorf("Interrupt on !Alive process = true, want false")
	}

	select {
	case err := <-sendErr:
		if err == nil {
			t.Error("Send returned nil err after Interrupt; expected ctx.Canceled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send() did not unblock after Interrupt — idle-branch " +
			"sendCancel.Load() was skipped (RNEW-006 regression)")
	}
}

// TestInterrupt_ConcurrentSendRace stresses concurrent Interrupt × Send
// on an idle-branch process under -race. Every Send here is blocked on
// ctx.Done(); every Interrupt falls into the dead-branch and must still
// unblock its peer Send via sendCancel. Verifies (a) no races on
// sendCancel atomics, (b) no goroutine leaks behind a never-cancelled ctx.
func TestInterrupt_ConcurrentSendRace(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("stress loop; skipped under -short")
	}

	proc := NewTestProcess()
	// Fix the process to the dead-branch shape for the whole run. We do
	// NOT flip AliveVal concurrently — that would be a test-only race on
	// a non-atomic field unrelated to the RNEW-006 invariant under test.
	// Keeping AliveVal=false throughout exercises the exact window the
	// original bug described: Send() enters, Store(sendCancel); Interrupt
	// observes proc != nil && !proc.Alive() and must still cancel.
	proc.AliveVal = false
	proc.SendFunc = func(ctx context.Context, _ string, _ []cli.ImageData, _ cli.EventCallback) (*cli.SendResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s := &ManagedSession{key: "rnew006-race"}
	s.storeProcess(proc)
	s.touchLastActive()

	var wg sync.WaitGroup
	const iters = 200

	for i := 0; i < iters; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			// Each Send is serialised by sendMu; the test only cares that
			// each one unblocks within the budget, not that each Send is
			// paired one-to-one with the Interrupt goroutine launched on
			// the same iteration — an Interrupt firing while goroutine N
			// is queued behind sendMu may still cancel goroutine N-k's
			// ctx, which drains the sendMu queue and is sufficient for
			// the "no goroutine leak" assertion.
			_, _ = s.Send(ctx, "msg", nil, nil)
		}()
		go func() {
			defer wg.Done()
			if i%4 == 0 {
				runtime.Gosched()
			}
			s.Interrupt()
		}()
	}

	// All goroutines must exit within a bounded budget; hanging Send()s
	// would indicate a lost cancel from the idle branch.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutine leak: Send/Interrupt pairs did not all drain " +
			"— idle-branch cancel likely missed (RNEW-006)")
	}
}

// TestInterrupt_IdleBranchSendCancelContract_SourceLevel is a static
// guard on the fix location. It verifies the dead-process early-return
// inside Interrupt() loads sendCancel and invokes it. A future edit that
// removes this handling reopens RNEW-006 without failing any behavioural
// test that happens to schedule favourably; this test catches it at the
// source level.
func TestInterrupt_IdleBranchSendCancelContract_SourceLevel(t *testing.T) {
	t.Parallel()
	_, thisFile, _, _ := runtime.Caller(0)
	src := filepath.Join(filepath.Dir(thisFile), "managed.go")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read managed.go: %v", err)
	}
	body := string(data)

	const sig = "func (s *ManagedSession) Interrupt() bool {"
	start := strings.Index(body, sig)
	if start < 0 {
		t.Fatalf("could not locate Interrupt() in managed.go")
	}
	rest := body[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("could not locate Interrupt() body end")
	}
	fn := rest[:end]

	// Split the function at the `proc.Interrupt()` pivot. The part BEFORE
	// is the dead-branch early return; the part AFTER is the alive path.
	pivot := strings.Index(fn, "proc.Interrupt()")
	if pivot < 0 {
		t.Fatalf("could not locate proc.Interrupt() pivot in Interrupt() body")
	}
	deadBranch := fn[:pivot]

	// The dead branch must contain a sendCancel.Load() + invocation so a
	// Send() blocked on ctx.Done() is unblocked even when the process
	// went dead/idle between Send storing sendCancel and Interrupt
	// reading loadProcess.
	if !strings.Contains(deadBranch, "sendCancel.Load()") {
		t.Error("Interrupt() dead-branch does not load sendCancel — " +
			"RNEW-006 regression. A Send() blocked on ctx.Done() will " +
			"hang forever when proc becomes !Alive between Send's " +
			"sendCancel.Store and Interrupt's loadProcess.")
	}
	// Must also invoke the loaded cancel. Loose suffix match `cancel)()`
	// accepts both the current `(*cancel)()` pointer-deref form and a
	// future refactor that stores CancelFunc directly (`cancel)()` would
	// still appear as part of `(*cancel)()` or a renamed local). A stale
	// check like `_ = s.sendCancel.Load()` would pass the Load grep above
	// but not this one.
	if !strings.Contains(deadBranch, "cancel)()") {
		t.Error("Interrupt() dead-branch loads sendCancel but never invokes it")
	}
}
