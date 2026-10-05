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

// TestTakeoverPrecheck pins #3315: the precheck refuses exactly the
// takeovers Takeover itself refuses before spawning. Each case runs the
// precheck and then a real Takeover on the same router; the spawn fails on
// newTestRouter's missing CLI, so a takeover that got past its gates
// returns a spawn error and no refusal sentinel.
func TestTakeoverPrecheck(t *testing.T) {
	const key = "dashboard:takeover:proj:general"
	sentinels := []error{ErrMaxProcs, ErrSpawnInFlight, ErrRouterStopped}
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
			gen, active := r.ss.Gen(), r.ss.Active()

			perr := r.TakeoverPrecheck(key, AgentOpts{})
			if tc.want == nil && perr != nil || tc.want != nil && !errors.Is(perr, tc.want) {
				t.Fatalf("TakeoverPrecheck = %v, want %v", perr, tc.want)
			}
			if r.ss.Gen() != gen || r.ss.Active() != active {
				t.Fatalf("precheck changed the table: gen %d→%d, active %d→%d", gen, r.ss.Gen(), active, r.ss.Active())
			}

			_, terr := r.Takeover(context.Background(), key, "", "/tmp/precheck-ws", AgentOpts{})
			if terr == nil {
				t.Fatal("Takeover succeeded; the test router cannot spawn")
			}
			for _, s := range sentinels {
				if errors.Is(perr, s) != errors.Is(terr, s) {
					t.Errorf("precheck %v and Takeover %v disagree on %v", perr, terr, s)
				}
			}
		})
	}
}

// TestTakeoverPrecheck_ExemptAndOpts pins #3395: for a planner's exempt opts the
// precheck applies reserveSpawn's exempt quotas instead of maxProcs, and it
// rejects the opts Takeover rejects, each agreeing with a real Takeover.
func TestTakeoverPrecheck_ExemptAndOpts(t *testing.T) {
	const key = "project:proj:planner"
	sentinels := []error{ErrMaxProcs, ErrSpawnInFlight, ErrRouterStopped, ErrMaxExemptSessions, ErrInvalidModel, ErrInvalidBackend}
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
			gen, active := r.ss.Gen(), r.ss.Active()

			perr := r.TakeoverPrecheck(key, tc.opts)
			if tc.want == nil && perr != nil || tc.want != nil && !errors.Is(perr, tc.want) {
				t.Fatalf("TakeoverPrecheck = %v, want %v", perr, tc.want)
			}
			if r.ss.Gen() != gen || r.ss.Active() != active {
				t.Fatalf("precheck changed the table: gen %d→%d, active %d→%d", gen, r.ss.Gen(), active, r.ss.Active())
			}

			_, terr := r.Takeover(context.Background(), key, "", "/tmp/precheck-ws", tc.opts)
			if terr == nil {
				t.Fatal("Takeover succeeded; the test router cannot spawn")
			}
			for _, s := range sentinels {
				if errors.Is(perr, s) != errors.Is(terr, s) {
					t.Errorf("precheck %v and Takeover %v disagree on %v", perr, terr, s)
				}
			}
		})
	}
}
