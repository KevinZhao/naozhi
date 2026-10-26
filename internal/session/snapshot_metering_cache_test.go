package session

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
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
	metering []clievent.MeteringEntry
	calls    atomic.Int64
}

func newMeteringGenTestProcess(gen uint64, metering []clievent.MeteringEntry) *meteringGenTestProcess {
	return &meteringGenTestProcess{TestProcess: NewTestProcess(), gen: gen, metering: metering}
}

func (m *meteringGenTestProcess) set(gen uint64, metering []clievent.MeteringEntry) {
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

func (m *meteringGenTestProcess) MeteringUsage() []clievent.MeteringEntry {
	m.calls.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.metering) == 0 {
		return nil
	}
	out := make([]clievent.MeteringEntry, len(m.metering))
	copy(out, m.metering)
	return out
}

func credit(v float64) []clievent.MeteringEntry {
	return []clievent.MeteringEntry{{Value: v, Unit: "credit", UnitPlural: "credits"}}
}

func newKiroSessionWith(proc processIface) *ManagedSession {
	s := &ManagedSession{key: "test:direct:alice:general"}
	s.SetBackend("kiro")
	s.storeProcess(proc)
	return s
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
