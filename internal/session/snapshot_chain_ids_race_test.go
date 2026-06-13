package session

import (
	"strconv"
	"sync"
	"testing"
)

// TestSnapshotChainIDs_ConcurrentReplace_NoRace reproduces the #2055 data
// race: the startup tier-2 history loader goroutine bare-read
// s.prevSessionIDs (slice header) while a cron stub refresh
// (RegisterCronStubWithChain → registerStub → ReplacePrevSessionIDs)
// reassigned that header under historyMu. The loader now goes through
// SnapshotChainIDs(), which takes historyMu.RLock, establishing the
// missing happens-before. Run with -race: a bare-read regression trips
// the detector here.
func TestSnapshotChainIDs_ConcurrentReplace_NoRace(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{}
	s.setSessionID("current")
	s.ReplacePrevSessionIDs([]string{"p0"})

	const iters = 200
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: mimics cron stub refresh reassigning the chain header.
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			chain := make([]string, 0, i%8+1)
			for j := 0; j <= i%8; j++ {
				chain = append(chain, "p"+strconv.Itoa(j))
			}
			s.ReplacePrevSessionIDs(chain)
		}
	}()

	// Reader: mimics the loader goroutine building the id list.
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			ids := s.SnapshotChainIDs()
			// Current is always non-empty at the loader site (it
			// continues past empty getSessionID()), so the snapshot
			// must always include the trailing current id.
			if len(ids) == 0 || ids[len(ids)-1] != "current" {
				t.Errorf("snapshot missing current id: %v", ids)
				return
			}
		}
	}()

	wg.Wait()
}
