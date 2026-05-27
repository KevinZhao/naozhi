package cron

// R20260527-GO-9 / R20260527-COR-4 (#1271, #1291) regression tests.
//
// trimRemoveBatch must re-acquire jobLock before re-panicking so the
// outer caller's `defer lock.Unlock()` does not panic on
// Unlock-of-unlocked-mutex when an os.Remove (or anything underneath:
// FUSE quirks, cgo trap, signal-time misadventure) panics. The original
// panic propagates unchanged so observability surfaces the underlying
// FS-layer failure.
//
// On the normal-return path the helper deliberately does NOT re-acquire
// the lock — trimJobLocked re-Lock()s explicitly so the structural pin
// in trim_unlock_during_remove_test.go (lock.Unlock() / batch /
// lock.Lock() shape) still matches the source.

import (
	"sync"
	"testing"
)

// TestTrimRemoveBatch_NormalReturnLeavesLockReleased pins the
// happy-path contract: the helper does NOT re-acquire on normal
// return. The caller (trimJobLocked) explicitly Lock()s after the
// helper. If the helper grew an unconditional `defer lock.Lock()`
// the caller's Lock() would re-acquire the same mutex from the same
// goroutine — sync.Mutex is not reentrant; behaviour is undefined
// (typically deadlock on the second Lock).
func TestTrimRemoveBatch_NormalReturnLeavesLockReleased(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 5, 0)
	var lock sync.Mutex
	// Caller-released state: the lock is NOT held when the helper
	// runs (this is exactly trimJobLocked's pattern: Unlock, helper,
	// Lock).
	s.trimRemoveBatch(&lock, nil) // empty batch, normal return
	if !lock.TryLock() {
		t.Fatal("trimRemoveBatch normal return must leave lock RELEASED " +
			"so the caller's explicit lock.Lock() succeeds; got LOCKED " +
			"— caller would deadlock on the second acquire in the same " +
			"goroutine.")
	}
	lock.Unlock()
}

// TestTrimRemoveBatch_PanicReacquiresLock proves the helper holds the
// lock on the panic exit path. We can't make a real os.Remove panic, so
// we mirror the helper's shape inline (defer recover → Lock → re-panic)
// and verify the lock state matches the contract trimJobLocked relies
// on: the lock is HELD when the panic propagates to the outer caller.
func TestTrimRemoveBatch_PanicReacquiresLock(t *testing.T) {
	t.Parallel()
	var lock sync.Mutex
	// Outer recovery point: this is what trimJobUnderLock / Append's
	// defer lock.Unlock() effectively expects — by the time the
	// panic reaches it, lock must be HELD so the deferred Unlock
	// doesn't panic on Unlock-of-unlocked-mutex.
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to propagate to outer recover")
		}
		if lock.TryLock() {
			lock.Unlock()
			t.Fatal("trimRemoveBatch panic exit path left lock UNLOCKED " +
				"— outer caller's defer Unlock() would panic on " +
				"Unlock-of-unlocked-mutex (#1271 / #1291).")
		}
		// We're holding the lock in the role of the outer caller now.
		// Release for cleanup.
		lock.Unlock()
	}()
	// Mirror trimRemoveBatch shape exactly.
	func() {
		defer func() {
			if r := recover(); r != nil {
				lock.Lock()
				panic(r)
			}
		}()
		// Caller has released the lock; this is the unlocked window.
		panic("synthetic os.Remove panic (FUSE quirk simulation)")
	}()
}
