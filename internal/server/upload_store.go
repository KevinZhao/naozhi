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
)

type uploadEntry struct {
	Image   cli.ImageData
	Created time.Time
}

// uploadStore holds pre-uploaded images keyed by random ID.
// Entries expire after uploadTTL and are cleaned up periodically.
type uploadStore struct {
	mu      sync.Mutex
	entries map[string]*uploadEntry
}

func newUploadStore() *uploadStore {
	return &uploadStore{entries: make(map[string]*uploadEntry)}
}

var errUploadStoreFull = errors.New("upload store full")

// Put stores an image and returns a random hex ID.
// Returns errUploadStoreFull when the store is at capacity.
func (s *uploadStore) Put(img cli.ImageData) (string, error) {
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
	s.entries[id] = &uploadEntry{Image: img, Created: time.Now()}
	return id, nil
}

// Take retrieves and removes an image by ID. Returns nil if not found or expired.
func (s *uploadStore) Take(id string) *cli.ImageData {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return nil
	}
	if time.Since(e.Created) > uploadTTL {
		delete(s.entries, id)
		return nil
	}
	delete(s.entries, id)
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
			delete(s.entries, id)
		}
	}
}
