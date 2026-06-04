package server

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/naozhi/naozhi/internal/discovery"
)

// mkSessionsDir creates an empty ~/.claude/sessions dir under a temp claudeDir
// and returns (claudeDir, sessionsDirMtime). tryShortCircuit stats this dir and
// compares its mtime against dc.lastDirMtime to decide whether to short-circuit.
func mkSessionsDir(t *testing.T) (string, time.Time) {
	t.Helper()
	claudeDir := t.TempDir()
	sessDir := filepath.Join(claudeDir, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	info, err := os.Stat(sessDir)
	if err != nil {
		t.Fatalf("stat sessions: %v", err)
	}
	return claudeDir, info.ModTime()
}

// sliceData returns the backing-array address of a DiscoveredSession slice so a
// test can assert whether two slices share storage (i.e. whether a republish
// allocated a fresh array).
func sliceData(s []discovery.DiscoveredSession) uintptr {
	if cap(s) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&s[:1][0]))
}

// TestTryShortCircuit_IdleNoRepublish pins the #1700 steady-state win: when the
// sessions dir is unchanged, all PIDs are alive, and no dynamic field changed
// (no JSONL on disk → RefreshDynamic returns changed=false), tryShortCircuit
// must NOT allocate or republish dc.sessions — the published backing array
// stays byte-identical across ticks. Before the fix every tick allocated a
// full N-element copy unconditionally.
func TestTryShortCircuit_IdleNoRepublish(t *testing.T) {
	t.Parallel()
	claudeDir, mtime := mkSessionsDir(t)

	dc := newDiscoveryCache(claudeDir, func() (map[int]bool, map[string]bool, map[string]bool) {
		return nil, nil, nil
	}, nil)

	// Seed a stable cached snapshot. PID = our own process so PidAlive is true.
	// No JSONL files exist for these sessions, so RefreshDynamic finds nothing
	// to change (LastActive already equals StartedAt via the fallback) and
	// returns changed=false.
	self := os.Getpid()
	old := time.Now().Add(-60 * time.Second).UnixMilli()
	seed := []discovery.DiscoveredSession{
		{PID: self, SessionID: "s1", CWD: "/tmp/a", StartedAt: old, LastActive: old, State: "ready"},
		{PID: self, SessionID: "s2", CWD: "/tmp/b", StartedAt: old, LastActive: old, State: "ready"},
	}
	dc.mu.Lock()
	dc.sessions = seed
	dc.lastDirMtime = mtime
	dc.mu.Unlock()

	before := sliceData(dc.sessions)

	if !dc.tryShortCircuit() {
		t.Fatal("tryShortCircuit returned false; expected short-circuit (dir unchanged + PIDs alive)")
	}

	after := sliceData(dc.sessions)
	if before != after {
		t.Errorf("dc.sessions backing array changed on an idle tick (before=%#x after=%#x) — "+
			"the #1700 changed-gate should have skipped the republish/allocation", before, after)
	}
}

// TestTryShortCircuit_ScratchReusedAcrossTicks pins that repeated idle ticks
// reuse the same refreshScratch backing array rather than allocating a new one
// each time. The first tick may grow it from nil; subsequent ticks must keep
// the same storage.
func TestTryShortCircuit_ScratchReusedAcrossTicks(t *testing.T) {
	t.Parallel()
	claudeDir, mtime := mkSessionsDir(t)

	dc := newDiscoveryCache(claudeDir, func() (map[int]bool, map[string]bool, map[string]bool) {
		return nil, nil, nil
	}, nil)

	self := os.Getpid()
	old := time.Now().Add(-60 * time.Second).UnixMilli()
	dc.mu.Lock()
	dc.sessions = []discovery.DiscoveredSession{
		{PID: self, SessionID: "s1", CWD: "/tmp/a", StartedAt: old, LastActive: old, State: "ready"},
	}
	dc.lastDirMtime = mtime
	dc.mu.Unlock()

	dc.refreshMu.Lock()
	_ = dc.tryShortCircuit()
	firstScratch := sliceData(dc.refreshScratch)
	dc.refreshMu.Unlock()

	if firstScratch == 0 {
		t.Fatal("refreshScratch was not populated after first idle tick")
	}

	dc.refreshMu.Lock()
	_ = dc.tryShortCircuit()
	secondScratch := sliceData(dc.refreshScratch)
	dc.refreshMu.Unlock()

	if firstScratch != secondScratch {
		t.Errorf("refreshScratch backing array changed between idle ticks "+
			"(first=%#x second=%#x) — scratch is not being reused", firstScratch, secondScratch)
	}
}

// TestTryShortCircuit_PublishedSliceNeverScratch pins the immutability contract:
// even when a republish happens, the slice handed to readers must NOT be the
// reusable scratch buffer (which the next tick would overwrite in place). We
// force a republish by making a PID die would change the list — instead we
// assert the structural invariant directly: after any tryShortCircuit, if
// refreshScratch is non-empty, dc.sessions must not alias it.
func TestTryShortCircuit_PublishedSliceNeverScratch(t *testing.T) {
	t.Parallel()
	claudeDir, mtime := mkSessionsDir(t)

	dc := newDiscoveryCache(claudeDir, func() (map[int]bool, map[string]bool, map[string]bool) {
		return nil, nil, nil
	}, nil)

	self := os.Getpid()
	old := time.Now().Add(-60 * time.Second).UnixMilli()
	dc.mu.Lock()
	dc.sessions = []discovery.DiscoveredSession{
		{PID: self, SessionID: "s1", CWD: "/tmp/a", StartedAt: old, LastActive: old, State: "ready"},
	}
	dc.lastDirMtime = mtime
	dc.mu.Unlock()

	dc.refreshMu.Lock()
	_ = dc.tryShortCircuit()
	scratch := sliceData(dc.refreshScratch)
	published := sliceData(dc.sessions)
	dc.refreshMu.Unlock()

	if scratch != 0 && scratch == published {
		t.Errorf("dc.sessions aliases the reusable scratch buffer (%#x) — "+
			"a future in-place RefreshDynamic would corrupt a reader's view", scratch)
	}
}

// TestRefresh_ConcurrentReadersRace stresses the refresh path against
// concurrent snapshot() readers. Run under -race it flags any data race on the
// published backing array or refreshScratch. refreshMu single-flights refresh;
// readers go through the RWMutex.
func TestRefresh_ConcurrentReadersRace(t *testing.T) {
	t.Parallel()
	claudeDir, mtime := mkSessionsDir(t)

	dc := newDiscoveryCache(claudeDir, func() (map[int]bool, map[string]bool, map[string]bool) {
		return nil, nil, nil
	}, nil)

	self := os.Getpid()
	old := time.Now().Add(-60 * time.Second).UnixMilli()
	dc.mu.Lock()
	dc.sessions = []discovery.DiscoveredSession{
		{PID: self, SessionID: "s1", CWD: "/tmp/a", StartedAt: old, LastActive: old, State: "ready"},
		{PID: self, SessionID: "s2", CWD: "/tmp/b", StartedAt: old, LastActive: old, State: "ready"},
	}
	dc.lastDirMtime = mtime
	dc.mu.Unlock()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Two refresh goroutines mimic startLoop's initial + ticker pair, both
	// hammering tryShortCircuit through the single-flight refreshMu.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					dc.refresh()
				}
			}
		}()
	}

	// Readers copy the published snapshot and read every string field, which
	// would race a reader against an in-place mutation of the backing array.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					for _, s := range dc.snapshot() {
						_ = s.Summary + s.LastPrompt + s.State
					}
				}
			}
		}()
	}

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}
