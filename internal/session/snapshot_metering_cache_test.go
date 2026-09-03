package session

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// meteringGenTestProcess is a TestProcess wrapper whose metering rows are
// versioned by an explicit generation counter, mirroring cli.Process's
// MeteringGen contract (gen advances on every metering write). It counts
// MeteringUsage calls so tests can assert Snapshot's cache actually skips
// the defensive copy when the generation is unchanged (#2345).
type meteringGenTestProcess struct {
	*TestProcess
	mu       sync.Mutex
	gen      uint64
	metering []cli.MeteringEntry
	calls    atomic.Int64
}

func newMeteringGenTestProcess(gen uint64, metering []cli.MeteringEntry) *meteringGenTestProcess {
	return &meteringGenTestProcess{TestProcess: NewTestProcess(), gen: gen, metering: metering}
}

func (m *meteringGenTestProcess) set(gen uint64, metering []cli.MeteringEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gen = gen
	m.metering = metering
}

func (m *meteringGenTestProcess) MeteringGen() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen
}

func (m *meteringGenTestProcess) MeteringUsage() []cli.MeteringEntry {
	m.calls.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.metering) == 0 {
		return nil
	}
	out := make([]cli.MeteringEntry, len(m.metering))
	copy(out, m.metering)
	return out
}

func credit(v float64) []cli.MeteringEntry {
	return []cli.MeteringEntry{{Value: v, Unit: "credit", UnitPlural: "credits"}}
}

func newKiroSessionWith(proc processIface) *ManagedSession {
	s := &ManagedSession{key: "test:direct:alice:general"}
	s.SetBackend("kiro")
	s.storeProcess(proc)
	return s
}

// TestSnapshot_MeteringCache_ReusesSliceWhenGenUnchanged pins #2345: two
// Snapshot calls against a process whose MeteringGen has not moved must share
// the same backing array (no per-poll make+copy) and must not re-enter
// proc.MeteringUsage. The shared slice is read-only by contract — every
// consumer json.Marshals and drops it.
func TestSnapshot_MeteringCache_ReusesSliceWhenGenUnchanged(t *testing.T) {
	t.Parallel()
	proc := newMeteringGenTestProcess(1, credit(0.05))
	s := newKiroSessionWith(proc)

	a := s.Snapshot()
	b := s.Snapshot()

	if len(a.MeteringUsage) != 1 || len(b.MeteringUsage) != 1 {
		t.Fatalf("MeteringUsage lens = %d / %d, want 1 / 1", len(a.MeteringUsage), len(b.MeteringUsage))
	}
	if &a.MeteringUsage[0] != &b.MeteringUsage[0] {
		t.Errorf("unchanged gen must reuse the cached backing array; got distinct slices %p vs %p",
			&a.MeteringUsage[0], &b.MeteringUsage[0])
	}
	if got := proc.calls.Load(); got != 1 {
		t.Errorf("proc.MeteringUsage calls = %d, want 1 (second Snapshot must hit the cache)", got)
	}
	if a.TotalCost != 0.05 || b.TotalCost != 0.05 {
		t.Errorf("TotalCost = %v / %v, want 0.05 (credits derived from metering)", a.TotalCost, b.TotalCost)
	}
}

// TestSnapshot_MeteringCache_RefreshesOnGenChange: a metering write (gen bump)
// invalidates the cache — the next Snapshot copies fresh rows and recomputes
// the credits total.
func TestSnapshot_MeteringCache_RefreshesOnGenChange(t *testing.T) {
	t.Parallel()
	proc := newMeteringGenTestProcess(1, credit(0.05))
	s := newKiroSessionWith(proc)

	first := s.Snapshot()
	proc.set(2, credit(0.08))
	second := s.Snapshot()

	if first.MeteringUsage[0].Value != 0.05 {
		t.Errorf("first Value = %v, want 0.05", first.MeteringUsage[0].Value)
	}
	if second.MeteringUsage[0].Value != 0.08 {
		t.Errorf("after gen bump Value = %v, want 0.08 (stale cache served)", second.MeteringUsage[0].Value)
	}
	if second.TotalCost != 0.08 {
		t.Errorf("after gen bump TotalCost = %v, want 0.08 (stale credits cache)", second.TotalCost)
	}
	if got := proc.calls.Load(); got != 2 {
		t.Errorf("proc.MeteringUsage calls = %d, want 2", got)
	}
}

// TestSnapshot_MeteringCache_GenZeroBypassesCache: a process that never
// versions its metering (gen stays 0 — legacy fakes, or a backend that has not
// reported metering yet) must be read live every poll. Otherwise a fake whose
// rows change without a gen bump would serve stale data.
func TestSnapshot_MeteringCache_GenZeroBypassesCache(t *testing.T) {
	t.Parallel()
	proc := newMeteringGenTestProcess(0, credit(0.05))
	s := newKiroSessionWith(proc)

	_ = s.Snapshot()
	proc.set(0, credit(0.07))
	snap := s.Snapshot()

	if len(snap.MeteringUsage) != 1 || snap.MeteringUsage[0].Value != 0.07 {
		t.Errorf("gen 0 must bypass the cache; got %+v, want value 0.07", snap.MeteringUsage)
	}
	if snap.TotalCost != 0.07 {
		t.Errorf("TotalCost = %v, want 0.07", snap.TotalCost)
	}
}

// TestSnapshot_MeteringCache_ProcessReplaceInvalidates: a respawned process
// starts its own generation counter, so gen alone cannot key the cache — the
// same gen value on a different process must not serve the old rows.
func TestSnapshot_MeteringCache_ProcessReplaceInvalidates(t *testing.T) {
	t.Parallel()
	old := newMeteringGenTestProcess(1, credit(0.05))
	s := newKiroSessionWith(old)
	_ = s.Snapshot()

	replacement := newMeteringGenTestProcess(1, credit(0.09))
	s.storeProcess(replacement)
	snap := s.Snapshot()

	if len(snap.MeteringUsage) != 1 || snap.MeteringUsage[0].Value != 0.09 {
		t.Errorf("replaced process with equal gen served stale rows: %+v, want 0.09", snap.MeteringUsage)
	}
	if snap.TotalCost != 0.09 {
		t.Errorf("TotalCost = %v, want 0.09", snap.TotalCost)
	}
}

// TestSnapshot_MeteringCache_ConcurrentReadersAndWriter runs Snapshot from
// several pollers while a writer keeps bumping the generation. Meant for
// -race: the cache publication must be lock-free-safe, and every snapshot
// must be self-consistent (the cached credits total always matches the
// cached rows it was derived from).
func TestSnapshot_MeteringCache_ConcurrentReadersAndWriter(t *testing.T) {
	t.Parallel()
	proc := newMeteringGenTestProcess(1, credit(1))
	s := newKiroSessionWith(proc)

	const writes = 200
	const readers = 4
	var wg sync.WaitGroup
	wg.Add(1 + readers)
	go func() {
		defer wg.Done()
		for i := 2; i <= writes; i++ {
			proc.set(uint64(i), credit(float64(i)))
		}
	}()
	errs := make(chan string, readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < writes; i++ {
				snap := s.Snapshot()
				if len(snap.MeteringUsage) != 1 {
					errs <- "MeteringUsage len != 1"
					return
				}
				if snap.TotalCost != snap.MeteringUsage[0].Value {
					errs <- "TotalCost does not match the metering rows it was derived from"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	final := s.Snapshot()
	if final.MeteringUsage[0].Value != writes {
		t.Errorf("final Value = %v, want %d (last write must be visible)", final.MeteringUsage[0].Value, writes)
	}
}

// BenchmarkSnapshot_WithMetering measures the steady-state Snapshot cost for
// a metering-bearing (kiro-class) session whose metering has not changed —
// the dominant 1 Hz × N tabs × M sessions poll case #2345 targets.
func BenchmarkSnapshot_WithMetering(b *testing.B) {
	proc := newMeteringGenTestProcess(1, []cli.MeteringEntry{
		{Value: 1.5, Unit: "credit", UnitPlural: "credits"},
		{Value: 1200, Unit: "token", UnitPlural: "tokens"},
		{Value: 0.02, Unit: "USD"},
	})
	s := newKiroSessionWith(proc)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Snapshot()
	}
}
