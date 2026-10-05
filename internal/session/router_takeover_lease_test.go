package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// reserveAndTakeover is a takeover of key with no external CLI to stop
// between the reservation and the spawn.
func reserveAndTakeover(ctx context.Context, r *Router, key, sessionID, workspace string, opts AgentOpts) (*ManagedSession, error) {
	lease, err := r.ReserveTakeover(key, opts)
	if err != nil {
		return nil, err
	}
	return r.Takeover(ctx, lease, sessionID, workspace)
}

// newLeaseTestRouter is a takeover router whose spawns succeed with an idle
// process, counted in spawns; onSpawn, when set, runs inside each spawn.
func newLeaseTestRouter(maxProcs int, spawns *atomic.Int32, onSpawn func()) *Router {
	r := newTakeoverTestRouter(maxProcs)
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) {
		spawns.Add(1)
		if onSpawn != nil {
			onSpawn()
		}
		return newIdleProc(), nil
	}
	return r
}

// assertNothingHeld fails when key still has an in-flight marker or the
// router a pending slot.
func assertNothingHeld(t *testing.T, r *Router, key string) {
	t.Helper()
	if _, held := spawnInFlight(r, key); held {
		t.Errorf("%s still has an in-flight marker", key)
	}
	if n := pendingSpawns(r); n != 0 {
		t.Errorf("%d pending spawn slots, want 0", n)
	}
}

// TestReserveTakeover_HoldsKeyUntilTakeover pins #3417: from the reserve to
// the spawn the key is held, so a second takeover is refused before its
// caller kills another CLI and a GetOrCreate parks for the taken-over session.
func TestReserveTakeover_HoldsKeyUntilTakeover(t *testing.T) {
	t.Parallel()
	const key = "dashboard:takeover:proj:general"
	var spawns atomic.Int32
	var lease *TakeoverLease
	var heldDuringSpawn atomic.Bool
	var r *Router
	r = newLeaseTestRouter(3, &spawns, func() {
		// Takeover has consumed the lease: a late Release must not end the
		// marker the spawn now owns.
		lease.Release()
		_, held := spawnInFlight(r, key)
		heldDuringSpawn.Store(held)
	})

	var err error
	if lease, err = r.ReserveTakeover(key, AgentOpts{}); err != nil {
		t.Fatalf("ReserveTakeover: %v", err)
	}
	if _, err := r.ReserveTakeover(key, AgentOpts{}); !errors.Is(err, ErrSpawnInFlight) {
		t.Fatalf("second ReserveTakeover = %v, want ErrSpawnInFlight", err)
	}
	if n := pendingSpawns(r); n != 1 {
		t.Fatalf("%d pending slots during the lease, want 1", n)
	}

	type got struct {
		s      *ManagedSession
		status SessionStatus
		err    error
	}
	getDone := make(chan got, 1)
	go func() {
		s, status, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
		getDone <- got{s, status, err}
	}()
	select {
	case g := <-getDone:
		t.Fatalf("GetOrCreate returned during the lease (%v, %v) instead of parking", g.status, g.err)
	case <-time.After(100 * time.Millisecond):
	}

	took, err := r.Takeover(context.Background(), lease, "sess-external", t.TempDir())
	if err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	g := <-getDone
	if g.err != nil || g.s != took || g.status != SessionExisting {
		t.Errorf("parked GetOrCreate got %p (%v, %v), want Takeover's session %p as existing", g.s, g.status, g.err, took)
	}
	if n := spawns.Load(); n != 1 {
		t.Errorf("%d spawns, want 1 (Takeover's)", n)
	}
	if !heldDuringSpawn.Load() {
		t.Error("Release of a consumed lease ended the spawn's marker")
	}
	assertNothingHeld(t, r, key)
	if _, err := r.Takeover(context.Background(), lease, "sess-external", t.TempDir()); !errors.Is(err, errLeaseSpent) {
		t.Errorf("Takeover on a consumed lease = %v, want errLeaseSpent", err)
	}
}

// TestReserveTakeover_SlotCountsAgainstMaxProcs pins #3417's other race: a
// lease holds a maxProcs slot while its CLI exits, so a second takeover or a
// spawn of another key cannot take the last slot from under it.
func TestReserveTakeover_SlotCountsAgainstMaxProcs(t *testing.T) {
	t.Parallel()
	var spawns atomic.Int32
	r := newLeaseTestRouter(1, &spawns, nil)

	lease, err := r.ReserveTakeover("dashboard:takeover:a:general", AgentOpts{})
	if err != nil {
		t.Fatalf("ReserveTakeover a: %v", err)
	}
	if _, err := r.ReserveTakeover("dashboard:takeover:b:general", AgentOpts{}); !errors.Is(err, ErrMaxProcs) {
		t.Errorf("ReserveTakeover b = %v, want ErrMaxProcs", err)
	}
	if _, _, err := r.GetOrCreate(context.Background(), "feishu:direct:c:general", AgentOpts{}); !errors.Is(err, ErrMaxProcs) {
		t.Errorf("GetOrCreate c = %v, want ErrMaxProcs", err)
	}
	// A planner needs no maxProcs room, so its lease takes none.
	planner, err := r.ReserveTakeover("project:p:planner", AgentOpts{Exempt: true})
	if err != nil {
		t.Fatalf("exempt ReserveTakeover: %v", err)
	}
	if n := pendingSpawns(r); n != 1 {
		t.Errorf("%d pending slots with one exempt lease and one not, want 1", n)
	}
	planner.Release()

	lease.Release()
	lease.Release()
	assertNothingHeld(t, r, "dashboard:takeover:a:general")
	b, err := r.ReserveTakeover("dashboard:takeover:b:general", AgentOpts{})
	if err != nil {
		t.Fatalf("ReserveTakeover b after a's release: %v", err)
	}
	b.Release()
	if n := spawns.Load(); n != 0 {
		t.Errorf("%d spawns, want none", n)
	}
}

// TestTakeover_EndsTheLeaseOnFailure: a Takeover that fails, at any gate or
// in the spawn, leaves neither the marker nor the slot behind, either of
// which would refuse every later takeover or spawn of the key.
func TestTakeover_EndsTheLeaseOnFailure(t *testing.T) {
	t.Parallel()
	const key = "dashboard:takeover:fail:general"
	errSpawn := errors.New("spawn failed")
	cases := []struct {
		name string
		// before runs between the reserve and the Takeover.
		before func(r *Router, lease *TakeoverLease)
		spawn  error
		want   error
	}{
		{"router stopped meanwhile", func(r *Router, _ *TakeoverLease) { r.stopped.Store(true) }, nil, ErrRouterStopped},
		{"spawn fails", func(*Router, *TakeoverLease) {}, errSpawn, errSpawn},
		{"lease released", func(_ *Router, l *TakeoverLease) { l.Release() }, nil, errLeaseSpent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTakeoverTestRouter(3)
			var spawns atomic.Int32
			r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) {
				spawns.Add(1)
				if tc.spawn != nil {
					return nil, tc.spawn
				}
				return newIdleProc(), nil
			}
			lease, err := r.ReserveTakeover(key, AgentOpts{})
			if err != nil {
				t.Fatalf("ReserveTakeover: %v", err)
			}
			tc.before(r, lease)
			if _, err := r.Takeover(context.Background(), lease, "sess-external", t.TempDir()); !errors.Is(err, tc.want) {
				t.Fatalf("Takeover = %v, want %v", err, tc.want)
			}
			assertNothingHeld(t, r, key)
			if tc.spawn == nil && spawns.Load() != 0 {
				t.Errorf("%d spawns for a refused Takeover", spawns.Load())
			}
		})
	}
}
