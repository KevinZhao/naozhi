package sessiontable

import (
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

type extState struct{ n int }

func newExtTable() *Table[*sess, extState, extState] {
	return New[*sess, extState, extState](chatOf, hashOf, func(x *extState) extState { return *x })
}

// TestUpdate_ReleasesTheLockWhenTheCallbackPanics: a panic inside a
// transaction propagates to the caller and leaves the lock free, rather than
// wedging every later transaction.
func TestUpdate_ReleasesTheLockWhenTheCallbackPanics(t *testing.T) {
	tab := newExtTable()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not reach the caller")
			}
		}()
		tab.Update(func(tx Tx[*sess, extState, extState]) { panic("boom") })
	}()
	if !tab.mu.TryLock() {
		t.Fatal("the lock stayed held after a panicking Update")
	}
	tab.mu.Unlock()
}

func TestTx_UnusableAfterItsCallback(t *testing.T) {
	tab := newExtTable()
	var kept Tx[*sess, extState, extState]
	tab.Update(func(tx Tx[*sess, extState, extState]) { kept = tx })
	defer func() {
		if recover() == nil {
			t.Error("a Tx kept past its callback still worked")
		}
	}()
	kept.Put("c:a", &sess{})
}

// TestTx_UnlockedReleasesTheLock: work inside Unlocked runs with the lock
// free, and the transaction holds it again afterwards.
func TestTx_UnlockedReleasesTheLock(t *testing.T) {
	tab := newExtTable()
	tab.Update(func(tx Tx[*sess, extState, extState]) {
		var free bool
		tx.Unlocked(func() {
			if tab.mu.TryLock() {
				free = true
				tab.mu.Unlock()
			}
		})
		if !free {
			t.Error("the lock was held inside Unlocked")
		}
		if tab.mu.TryLock() {
			t.Error("the lock was not taken again after Unlocked")
		}
		tx.Put("c:a", &sess{}) // still a working Tx
	})
	if tab.size() != 1 {
		t.Error("the Put after Unlocked did not land")
	}
}

func TestView_ConcurrentReaders(t *testing.T) {
	tab := newExtTable()
	inside := make(chan struct{})
	release := make(chan struct{})
	go tab.View(func(v View[*sess, extState, extState]) {
		close(inside)
		<-release
	})
	<-inside
	done := make(chan struct{})
	go tab.View(func(v View[*sess, extState, extState]) { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a second View waited for the first")
	}
	close(release)
}

// TestTx_ExtChangesWithTheTable: the extension state is guarded by the same
// lock, so concurrent updates to it never interleave.
func TestTx_ExtChangesWithTheTable(t *testing.T) {
	tab := newExtTable()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tab.Update(func(tx Tx[*sess, extState, extState]) { tx.Ext().n++ })
		}()
	}
	wg.Wait()
	var n int
	tab.View(func(v View[*sess, extState, extState]) { n = v.Ext().n })
	if n != 50 {
		t.Errorf("ext counter = %d, want 50", n)
	}
}

// TestTx_WaitReleasesUntilBroadcast mirrors Shutdown: wait inside a
// transaction for another one to change the condition.
func TestTx_WaitReleasesUntilBroadcast(t *testing.T) {
	tab := newExtTable()
	waiting := make(chan struct{})
	go func() {
		<-waiting
		// Takes the lock only once the waiter's Wait has released it.
		tab.Update(func(tx Tx[*sess, extState, extState]) {
			tx.Ext().n = 1
			tx.Broadcast()
		})
	}()
	tab.Update(func(tx Tx[*sess, extState, extState]) {
		close(waiting)
		for tx.Ext().n == 0 {
			tx.Wait()
		}
	})
}

// TestTx_MutatorsPanicInsideUnlocked: the Tx does not hold the lock inside
// Unlocked, so its mutators refuse to run there.
func TestTx_MutatorsPanicInsideUnlocked(t *testing.T) {
	tab := newExtTable()
	tab.Update(func(tx Tx[*sess, extState, extState]) {
		tx.Unlocked(func() {
			defer func() {
				if recover() == nil {
					t.Error("a Tx mutator ran inside Unlocked")
				}
			}()
			tx.Put("c:a", &sess{})
		})
		tx.Put("c:b", &sess{}) // live again once the lock is back
	})
	if _, ok := tab.lookup("c:a"); ok {
		t.Error("the refused Put landed")
	}
	if _, ok := tab.lookup("c:b"); !ok {
		t.Error("the Put after Unlocked did not land")
	}
}

// TestTx_StaleTxFromAnInterleavedUpdateIsRefused: a Tx kept from an Update
// that ran inside another's Unlocked window stays dead after the window.
func TestTx_StaleTxFromAnInterleavedUpdateIsRefused(t *testing.T) {
	tab := newExtTable()
	var stale Tx[*sess, extState, extState]
	tab.Update(func(tx Tx[*sess, extState, extState]) {
		tx.Unlocked(func() {
			tab.Update(func(inner Tx[*sess, extState, extState]) { stale = inner })
		})
		defer func() {
			if recover() == nil {
				t.Error("a Tx from an interleaved Update still worked")
			}
		}()
		stale.Put("c:x", &sess{})
	})
}

func TestTable_LoadAndCount(t *testing.T) {
	tab := newExtTable()
	a := &sess{}
	tab.Update(func(tx Tx[*sess, extState, extState]) {
		tx.Put("c:a", a)
		tx.Put("c:b", &sess{})
		tx.SetActive(1)
	})
	if tab.Load("c:a") != a || tab.Load("c:missing") != nil {
		t.Error("Load")
	}
	if n, act := tab.Count(); n != 2 || act != 1 {
		t.Errorf("Count = (%d,%d), want (2,1)", n, act)
	}
}

// TestTable_ExportedSurfaceIsTransactionsOnly: nothing reaches the table's
// state except a transaction, the two single-value reads and the lock-free
// atomics. A method added beside them would be a way around the lock.
func TestTable_ExportedSurfaceIsTransactionsOnly(t *testing.T) {
	typ := reflect.TypeOf(newTable())
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	want := []string{"Active", "BumpGen", "Count", "Gen", "Healthy", "Load", "Update", "View"}
	if !slices.Equal(got, want) {
		t.Errorf("exported methods = %v, want %v", got, want)
	}
}

// A read transaction gets the caller's read-only view of Ext (what New's
// readOnly returns), a write transaction the state itself.
func TestExt_ViewGetsTheReadOnlyView(t *testing.T) {
	calls := 0
	tab := New[*sess, extState](chatOf, hashOf, func(x *extState) int { calls++; return x.n * 10 })
	tab.Update(func(tx Tx[*sess, extState, int]) { tx.Ext().n = 4 })
	var got int
	tab.View(func(v View[*sess, extState, int]) { got = v.Ext() })
	if got != 40 || calls != 1 {
		t.Errorf("View.Ext() = %d after %d readOnly calls, want 40 after 1", got, calls)
	}
}
