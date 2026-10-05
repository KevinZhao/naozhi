package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// injectExempt installs an exempt session, which never counts as active.
func injectExempt(r *Router, key string, proc processIface) {
	r.ss.Update(func(tx sessTx) {
		s := &ManagedSession{key: key, exempt: true}
		s.storeProcess(proc)
		s.touchLastActive()
		tx.Put(key, s)
	})
}

// checkReserveAgainstSpawn reserves key and holds the answer to what the
// spawn gates say. A refusal must leave the table as it was and match the
// error reserveSpawn gives the same takeover reaching it unreserved; a lease
// must take a Takeover past every gate to newTestRouter's missing CLI, so it
// fails with a spawn error and no refusal sentinel, and leave nothing held.
// oracle false skips the unreserved run, for refusals of the opts themselves.
func checkReserveAgainstSpawn(t *testing.T, r *Router, key string, opts AgentOpts, want error, oracle bool) {
	t.Helper()
	sentinels := []error{ErrMaxProcs, ErrSpawnInFlight, ErrRouterStopped, ErrMaxExemptSessions, ErrInvalidModel, ErrInvalidBackend}
	gen, active, pending := r.ss.Gen(), r.ss.Active(), pendingSpawns(r)
	_, inflight := spawnInFlight(r, key)

	lease, perr := r.ReserveTakeover(key, opts)
	if want == nil && perr != nil || want != nil && !errors.Is(perr, want) {
		t.Fatalf("ReserveTakeover = %v, want %v", perr, want)
	}
	var terr error
	if perr != nil {
		if _, held := spawnInFlight(r, key); r.ss.Gen() != gen || r.ss.Active() != active || pendingSpawns(r) != pending || held != inflight {
			t.Fatalf("a refused reserve changed the table: gen %d→%d, active %d→%d, pending %d→%d, marker %v→%v",
				gen, r.ss.Gen(), active, r.ss.Active(), pending, pendingSpawns(r), inflight, held)
		}
		if !oracle {
			return
		}
		_, terr = r.Takeover(context.Background(), unreservedLease(r, key, opts), "", "/tmp/precheck-ws")
	} else {
		_, terr = r.Takeover(context.Background(), lease, "", "/tmp/precheck-ws")
		if terr == nil {
			t.Fatal("Takeover succeeded; the test router cannot spawn")
		}
		if _, held := spawnInFlight(r, key); held || pendingSpawns(r) != pending {
			t.Fatalf("after Takeover: marker %v, pending %d; want none and %d", held, pendingSpawns(r), pending)
		}
	}
	for _, s := range sentinels {
		if errors.Is(perr, s) != errors.Is(terr, s) {
			t.Errorf("ReserveTakeover %v and the spawn %v disagree on %v", perr, terr, s)
		}
	}
}

// unreservedLease is a lease that skipped ReserveTakeover's checks, so
// Takeover meets the spawn's own gates. A marker already on key stays its
// owner's: the lease's guard is then not the key's marker.
func unreservedLease(r *Router, key string, opts AgentOpts) *TakeoverLease {
	l := &TakeoverLease{key: key, opts: opts, slot: pendingSpawnSlot{r: r, released: true}}
	r.ss.Update(func(tx sessTx) {
		var owned bool
		if l.guard, owned = tx.Ext().spawns.BeginSpawn(key); !owned {
			l.guard = make(chan struct{})
		}
	})
	return l
}

func pendingSpawns(r *Router) (n int) {
	r.ss.View(func(v sessView) { n = v.Ext().PendingSpawns() })
	return n
}

func spawnInFlight(r *Router, key string) (ch chan struct{}, ok bool) {
	r.ss.View(func(v sessView) { ch, ok = v.Ext().SpawnInFlight(key) })
	return ch, ok
}

// TestReserveTakeover pins #3315: a reserve refuses exactly the takeovers the
// spawn refuses before starting the CLI (see checkReserveAgainstSpawn).
func TestReserveTakeover(t *testing.T) {
	const key = "dashboard:takeover:proj:general"
	cases := []struct {
		name     string
		maxProcs int
		setup    func(r *Router)
		want     error
	}{
		{"free slot", 2, func(r *Router) {
			injectSession(r, "a", newRunningProc())
		}, nil},
		{"at capacity, every session running", 2, func(r *Router) {
			injectSession(r, "a", newRunningProc())
			injectSession(r, "b", newRunningProc())
		}, ErrMaxProcs},
		{"at capacity, one idle session to evict", 2, func(r *Router) {
			injectSession(r, "a", newRunningProc())
			injectSession(r, "b", newIdleProc())
		}, nil},
		{"over capacity, one eviction is not enough", 1, func(r *Router) {
			injectSession(r, "a", newIdleProc())
			injectSession(r, "b", newIdleProc())
		}, ErrMaxProcs},
		{"at capacity, an alive session on the key frees its slot", 2, func(r *Router) {
			injectSession(r, "a", newRunningProc())
			injectSession(r, key, newRunningProc())
		}, nil},
		{"the key's own idle session is not an eviction victim", 1, func(r *Router) {
			injectSession(r, "a", newRunningProc())
			injectSession(r, key, newIdleProc())
		}, ErrMaxProcs},
		{"an exempt session on the key frees no slot", 1, func(r *Router) {
			injectSession(r, "a", newRunningProc())
			injectExempt(r, key, newIdleProc())
		}, ErrMaxProcs},
		{"exempt sessions hold no slot", 1, func(r *Router) {
			injectExempt(r, "cron:a", newRunningProc())
			injectExempt(r, "cron:b", newIdleProc())
		}, nil},
		{"dead sessions hold no slot", 1, func(r *Router) {
			injectSession(r, "a", newDeadProc())
		}, nil},
		{"pending spawns hold slots", 2, func(r *Router) {
			injectSession(r, "a", newRunningProc())
			r.ss.Update(func(tx sessTx) { tx.Ext().spawns.AcquireSpawnSlot() })
		}, ErrMaxProcs},
		{"pending spawn plus an idle session to evict", 2, func(r *Router) {
			injectSession(r, "a", newIdleProc())
			r.ss.Update(func(tx sessTx) { tx.Ext().spawns.AcquireSpawnSlot() })
		}, nil},
		{"pending spawn plus an idle session to evict, one eviction is not enough", 2, func(r *Router) {
			injectSession(r, "a", newIdleProc())
			injectSession(r, "b", newRunningProc())
			r.ss.Update(func(tx sessTx) { tx.Ext().spawns.AcquireSpawnSlot() })
		}, ErrMaxProcs},
		{"an active count below the table, the key's slot freed on top", 2, func(r *Router) {
			injectSession(r, "a", newRunningProc())
			injectSession(r, "b", newRunningProc())
			injectSession(r, key, newRunningProc())
			r.ss.Update(func(tx sessTx) { tx.AddActive(-1) })
		}, nil},
		{"an active count below the table admits", 2, func(r *Router) {
			injectSession(r, "a", newRunningProc())
			injectSession(r, "b", newRunningProc())
			r.ss.Update(func(tx sessTx) { tx.AddActive(-1) })
		}, nil},
		{"spawn in flight on the key", 3, func(r *Router) {
			r.ss.Update(func(tx sessTx) { tx.Ext().spawns.BeginSpawn(key) })
		}, ErrSpawnInFlight},
		{"spawn in flight on another key", 3, func(r *Router) {
			r.ss.Update(func(tx sessTx) { tx.Ext().spawns.BeginSpawn("other") })
		}, nil},
		{"router stopped", 3, func(r *Router) { r.stopped.Store(true) }, ErrRouterStopped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTakeoverTestRouter(tc.maxProcs)
			tc.setup(r)
			checkReserveAgainstSpawn(t, r, key, AgentOpts{}, tc.want, true)
		})
	}
}

// TestReserveTakeover_ExemptAndOpts pins #3395: for a planner's exempt opts a
// reserve applies reserveSpawn's exempt quotas instead of maxProcs, and it
// rejects the opts GetOrCreate rejects.
func TestReserveTakeover_ExemptAndOpts(t *testing.T) {
	const key = "project:proj:planner"
	exempt := AgentOpts{Exempt: true}
	injectN := func(r *Router, prefix string, n int) {
		for i := range n {
			injectExempt(r, fmt.Sprintf("%s%d", prefix, i), newIdleProc())
		}
	}
	cases := []struct {
		name  string
		opts  AgentOpts
		setup func(r *Router)
		want  error
	}{
		{"exempt bypasses a full maxProcs", exempt, func(r *Router) {
			injectSession(r, "a", newRunningProc())
		}, nil},
		{"the project quota is full", exempt, func(r *Router) {
			injectN(r, "project:p", maxProjectExempt)
		}, ErrMaxExemptSessions},
		{"the global exempt quota is full", exempt, func(r *Router) {
			injectN(r, "cron:c", maxExemptSessions)
		}, ErrMaxExemptSessions},
		{"an alive session on the key frees its project slot", exempt, func(r *Router) {
			injectN(r, "project:p", maxProjectExempt-1)
			injectExempt(r, key, newIdleProc())
		}, nil},
		{"an alive session on the key frees its global slot", exempt, func(r *Router) {
			injectN(r, "cron:c", maxExemptSessions-1)
			injectExempt(r, key, newIdleProc())
		}, nil},
		{"a non-exempt session on the key frees no exempt slot", exempt, func(r *Router) {
			injectN(r, "project:p", maxProjectExempt)
			injectSession(r, key, newIdleProc())
		}, ErrMaxExemptSessions},
		{"dead exempt sessions hold no slot", exempt, func(r *Router) {
			for i := range maxProjectExempt {
				injectExempt(r, fmt.Sprintf("project:p%d", i), newDeadProc())
			}
		}, nil},
		{"spawn in flight on the key", exempt, func(r *Router) {
			r.ss.Update(func(tx sessTx) { tx.Ext().spawns.BeginSpawn(key) })
		}, ErrSpawnInFlight},
		{"router stopped", exempt, func(r *Router) { r.stopped.Store(true) }, ErrRouterStopped},
		{"an invalid model", AgentOpts{Model: "--x"}, func(*Router) {}, ErrInvalidModel},
		{"an invalid backend", AgentOpts{Backend: "--x"}, func(*Router) {}, ErrInvalidBackend},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTakeoverTestRouter(1)
			tc.setup(r)
			optsRefused := errors.Is(tc.want, ErrInvalidModel) || errors.Is(tc.want, ErrInvalidBackend)
			checkReserveAgainstSpawn(t, r, key, tc.opts, tc.want, !optsRefused)
		})
	}
}
