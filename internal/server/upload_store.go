package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

const (
	uploadTTL         = 10 * time.Minute
	uploadCleanupFreq = 1 * time.Minute
	maxUploadEntries  = 100 // global cap to prevent memory exhaustion
	// maxUploadPerOwner caps how many live entries any single owner can
	// hold. Without this, one user leaving 100 un-claimed uploads (e.g.
	// a forgotten browser tab) starves every other user with 429 until
	// the next cleanup tick, up to 1 minute of wedging.
	maxUploadPerOwner = 20
)

type uploadEntry struct {
	Image   cli.ImageData
	Owner   string
	Created time.Time
}

// uploadStore holds pre-uploaded images keyed by random ID.
// Entries expire after uploadTTL and are cleaned up periodically.
//
// ownerCounts mirrors len(entries) partitioned by owner. Maintaining it
// in-band with Put/Take/evict lets the per-owner quota check run in O(1)
// instead of scanning all 100 entries on every upload; under a burst of
// small uploads the old linear path amplified lock hold time and starved
// the cleanup goroutine. Invariant: ownerCounts[o] == |{e | e.Owner==o}|
// for every live entry, and owner "" is intentionally not tracked (the
// per-owner cap is bypassed for unauthenticated requests — see Put).
type uploadStore struct {
	mu          sync.Mutex
	entries     map[string]*uploadEntry
	ownerCounts map[string]int
}

func newUploadStore() *uploadStore {
	return &uploadStore{
		entries:     make(map[string]*uploadEntry),
		ownerCounts: make(map[string]int),
	}
}

var (
	errUploadStoreFull = errors.New("upload store full")
	errUploadPerOwner  = errors.New("upload quota exceeded for this user")
)

// Put stores an image owned by owner and returns a random hex ID.
// Returns errUploadStoreFull when the global cap is hit, or
// errUploadPerOwner when the caller already has maxUploadPerOwner live entries.
func (s *uploadStore) Put(owner string, img cli.ImageData) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	id := hex.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= maxUploadEntries {
		return "", errUploadStoreFull
	}
	// Per-owner sub-limit: O(1) via ownerCounts map. Empty owner bypasses
	// the check — unauthenticated deployments (dashToken=="") fall through
	// with owner=="" and should not be rejected by this limit; callers
	// authenticated via token/cookie always produce a non-empty owner.
	if owner != "" && s.ownerCounts[owner] >= maxUploadPerOwner {
		return "", errUploadPerOwner
	}
	s.entries[id] = &uploadEntry{Image: img, Owner: owner, Created: time.Now()}
	if owner != "" {
		s.ownerCounts[owner]++
	}
	return id, nil
}

// removeEntryLocked decrements the owner counter and deletes the entry.
// Caller must hold s.mu. Keeping the bookkeeping in one place preserves
// the ownerCounts invariant across Take/evict paths.
func (s *uploadStore) removeEntryLocked(id string, e *uploadEntry) {
	delete(s.entries, id)
	if e.Owner != "" {
		if n := s.ownerCounts[e.Owner] - 1; n <= 0 {
			delete(s.ownerCounts, e.Owner)
		} else {
			s.ownerCounts[e.Owner] = n
		}
	}
}

// Take retrieves and removes an image by ID, verifying ownership.
// Returns nil if not found, expired, or owner does not match — callers
// receive the same "not found" response regardless of the failure reason
// to avoid leaking the existence of another user's upload.
func (s *uploadStore) Take(id, owner string) *cli.ImageData {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return nil
	}
	if time.Since(e.Created) > uploadTTL {
		s.removeEntryLocked(id, e)
		return nil
	}
	if e.Owner != owner {
		return nil
	}
	s.removeEntryLocked(id, e)
	return &e.Image
}

// StartCleanup runs periodic eviction of expired entries until ctx is cancelled.
func (s *uploadStore) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(uploadCleanupFreq)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.evict()
			}
		}
	}()
}

func (s *uploadStore) evict() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, e := range s.entries {
		if now.Sub(e.Created) > uploadTTL {
			s.removeEntryLocked(id, e)
		}
	}
}
