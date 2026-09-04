package session

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestSaveIfDirty_KnownIDsConcurrentTrackAndSnapshot pins the knownids lock
// contract (#2306, #2495): the periodic save (ClaimSave + memo-cache rebuild),
// lock-free snapshot readers and r.mu-held Track calls run concurrently and
// stay race-free because the store owns its mutex — no r.mu mode (RLock vs
// Lock) can make a cache write race a reader. Run with -race.
func TestSaveIfDirty_KnownIDsConcurrentTrackAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	r := &Router{
		ss:        sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs:  3,
		ttl:       30 * time.Minute,
		pruneTTL:  72 * time.Hour,
		storePath: storePath,
	}
	r.kid.Track("sess-0")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Saver: reopen the throttle and run saveIfDirty so ClaimSave and the
	// memoised marshal execute every iteration.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.kid.ResetSaveThrottle()
			r.saveIfDirty()
		}
	}()

	// Reader: snapshot the store without any Router lock, the way Shutdown
	// and the save path do.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = r.kid.SortedSnapshot()
			if _, err := r.kid.MarshalSnapshot(); err != nil {
				t.Errorf("MarshalSnapshot: %v", err)
				return
			}
			_ = r.kid.Dirty()
			_ = r.kid.Gen()
		}
	}()

	// Mutator: Track under r.mu as every publish site does (lock order
	// r.mu → kid.mu), bumping gen so the caches keep rebuilding.
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			i++
			r.mu.Lock()
			r.kid.Track(sessionIDForIter(i))
			r.mu.Unlock()
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Sanity: a final due save still persists correctly after the churn.
	r.kid.ResetSaveThrottle()
	r.saveIfDirty()
	if loaded := loadKnownIDs(storePath); len(loaded) == 0 {
		t.Error("known IDs not persisted after concurrent churn")
	}
}

func sessionIDForIter(i int) string {
	// Distinct IDs so each Track bumps gen.
	const hex = "0123456789abcdef"
	b := []byte("sess-xxxxxx")
	v := i
	for p := len(b) - 1; p >= len(b)-6; p-- {
		b[p] = hex[v&0xf]
		v >>= 4
	}
	return string(b)
}
