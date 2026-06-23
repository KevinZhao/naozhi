package session

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestSaveIfDirty_KnownIDsMarshalUnderWriteLock is the R202606g-GO-002 (#2306)
// regression guard. snapshotKnownIDsMarshaledLocked unconditionally writes the
// memoised cache fields (r.kid.sortedCache / sortedGen / marshaledCache /
// marshaledGen). saveIfDirty used to invoke it under r.mu.RLock(), which is a
// lock-contract violation: a write performed while only holding the read lock
// can race any concurrent RLock reader of those same fields.
//
// This test drives saveIfDirty (the memoised marshal path) concurrently with
// repeated read-lock snapshots of the known-ID cache via
// snapshotKnownIDsMarshaledLocked-equivalent readers, plus a stream of
// trackSessionID mutations that keep bumping r.kid.gen so the marshal cache is
// constantly rebuilt. Run with -race: pre-fix the cache write under RLock
// races the concurrent readers; post-fix the write is confined to the
// exclusive write-lock block and the run is clean.
func TestSaveIfDirty_KnownIDsMarshalUnderWriteLock(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	r := &Router{
		ss:        sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs:  3,
		ttl:       30 * time.Minute,
		pruneTTL:  72 * time.Hour,
		storePath: storePath,
		kid:       knownIDsStore{ids: map[string]bool{"sess-0": true}},
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer-side: repeatedly arm the known-IDs throttle and run saveIfDirty so
	// the memoised marshal (and its cache writes) execute every iteration.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.mu.Lock()
			r.kid.dirty = true
			r.kid.savedAt = time.Now().Add(-2 * knownIDsSaveInterval)
			r.mu.Unlock()
			r.saveIfDirty()
		}
	}()

	// Reader-side: take the read lock and read the memoised cache fields the
	// way other RLock readers do. Pre-fix this races the writer's cache write.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.mu.RLock()
			_ = r.kid.marshaledGen
			_ = len(r.kid.marshaledCache)
			_ = r.kid.sortedGen
			_ = len(r.kid.sortedCache)
			r.mu.RUnlock()
		}
	}()

	// Mutator: bump gen so the marshal cache is invalidated and rebuilt,
	// maximising the window in which the cache fields are written.
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
			r.trackSessionID(sessionIDForIter(i))
			r.mu.Unlock()
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Sanity: a final due save still persists correctly after the churn.
	r.mu.Lock()
	r.kid.dirty = true
	r.kid.savedAt = time.Now().Add(-2 * knownIDsSaveInterval)
	r.mu.Unlock()
	r.saveIfDirty()
	if loaded := loadKnownIDs(storePath); len(loaded) == 0 {
		t.Error("known IDs not persisted after concurrent churn")
	}
}

func sessionIDForIter(i int) string {
	// Distinct IDs so each trackSessionID bumps gen.
	const hex = "0123456789abcdef"
	b := []byte("sess-xxxxxx")
	v := i
	for p := len(b) - 1; p >= len(b)-6; p-- {
		b[p] = hex[v&0xf]
		v >>= 4
	}
	return string(b)
}
