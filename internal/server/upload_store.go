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
	maxUploadPerOwner = 40
	// maxFilesPerSend caps how many files (inline + pre-uploaded) a single
	// send can reference. Matched at the WS, HTTP JSON, and HTTP multipart
	// paths. Kept below maxUploadPerOwner so a full batch can still be
	// accompanied by an in-flight retry without tripping the per-owner quota.
	maxFilesPerSend = 20
	// maxUploadBytesPerOwner bounds the sum of live entry payload sizes
	// per owner. Added when PDFs joined the upload path: with images alone
	// the 10 MB * 40 entries = 400 MB per-owner worst case was tolerable;
	// with PDFs at up to 32 MB each the entry-count cap alone would permit
	// 32*40 = 1.28 GB resident per bad actor. This byte cap (96 MB ≈
	// 3 PDFs + many images) is the real safety rail while the entry count
	// stays a cheap O(1) guard for pathological many-small-file floods.
	maxUploadBytesPerOwner = 96 * 1024 * 1024 // 96 MB
	// maxUploadBytesGlobal caps the sum of all live entry payload sizes.
	// With maxUploadEntries at 100 and PDFs up to 32 MB, without this cap
	// the store could hold 3.2 GB resident even with the per-owner cap —
	// a handful of colluding owners could still starve the host. 512 MB
	// accommodates realistic bursts while preventing runaway growth.
	maxUploadBytesGlobal = 512 * 1024 * 1024 // 512 MB
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
//
// ownerBytes + totalBytes run on the same invariant principle for byte
// accounting. Byte caps fire BEFORE entry-count caps because the former is
// the real memory backstop; either tripping rejects the Put with the same
// errUploadPerOwner / errUploadStoreFull sentinels (callers don't need to
// distinguish count-vs-byte exhaustion — both are "try again later").
type uploadStore struct {
	mu          sync.Mutex
	entries     map[string]*uploadEntry
	ownerCounts map[string]int
	ownerBytes  map[string]int64
	totalBytes  int64
}

func newUploadStore() *uploadStore {
	return &uploadStore{
		entries:     make(map[string]*uploadEntry),
		ownerCounts: make(map[string]int),
		ownerBytes:  make(map[string]int64),
	}
}

var (
	errUploadStoreFull = errors.New("upload store full")
	errUploadPerOwner  = errors.New("upload quota exceeded for this user")
	// errUploadNotFound is returned by TakeAll when any id in the batch is
	// missing, expired, or owned by someone else. Callers should translate
	// to a single generic "file not found or expired" client-facing
	// message — the fid is user-supplied and MUST NOT be echoed back
	// (see R37-CONCUR4 and dashboard_send.go's existing rationale).
	errUploadNotFound = errors.New("file not found or expired")
)

// Put stores an image owned by owner and returns a random hex ID.
// Returns errUploadStoreFull when either the global entry cap or global
// byte cap is hit, or errUploadPerOwner when the caller's entry/byte
// sub-limit would be exceeded.
func (s *uploadStore) Put(owner string, img cli.ImageData) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	id := hex.EncodeToString(b)

	sz := entrySize(img)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= maxUploadEntries {
		return "", errUploadStoreFull
	}
	if s.totalBytes+sz > maxUploadBytesGlobal {
		return "", errUploadStoreFull
	}
	// Per-owner sub-limit: O(1) via ownerCounts map. Empty owner bypasses
	// the check — unauthenticated deployments (dashToken=="") fall through
	// with owner=="" and should not be rejected by this limit; callers
	// authenticated via token/cookie always produce a non-empty owner.
	if owner != "" {
		if s.ownerCounts[owner] >= maxUploadPerOwner {
			return "", errUploadPerOwner
		}
		if s.ownerBytes[owner]+sz > maxUploadBytesPerOwner {
			return "", errUploadPerOwner
		}
	}
	s.entries[id] = &uploadEntry{Image: img, Owner: owner, Created: time.Now()}
	s.totalBytes += sz
	if owner != "" {
		s.ownerCounts[owner]++
		s.ownerBytes[owner] += sz
	}
	return id, nil
}

// entrySize reports the payload byte count used for quota accounting. Only
// Data contributes — MimeType / OrigName / WorkspacePath are tiny and their
// overhead is comfortably absorbed by the per-entry budget. Counting
// everything would make the caps less predictable without meaningful
// defence benefit.
func entrySize(img cli.ImageData) int64 {
	return int64(len(img.Data))
}

// removeEntryLocked decrements the owner counter / byte accounting and
// deletes the entry. Caller must hold s.mu. Keeping the bookkeeping in
// one place preserves the ownerCounts / ownerBytes / totalBytes invariants
// across Take/evict paths.
func (s *uploadStore) removeEntryLocked(id string, e *uploadEntry) {
	delete(s.entries, id)
	sz := entrySize(e.Image)
	s.totalBytes -= sz
	if s.totalBytes < 0 {
		// Defensive: a negative total would surface as an integer
		// underflow wrap on int64 arithmetic later. The invariant says
		// this is unreachable, but pinning it at zero on the off chance
		// a future refactor breaks the invariant keeps the accounting
		// sane without a panic.
		s.totalBytes = 0
	}
	if e.Owner != "" {
		if n := s.ownerCounts[e.Owner] - 1; n <= 0 {
			delete(s.ownerCounts, e.Owner)
		} else {
			s.ownerCounts[e.Owner] = n
		}
		if b := s.ownerBytes[e.Owner] - sz; b <= 0 {
			delete(s.ownerBytes, e.Owner)
		} else {
			s.ownerBytes[e.Owner] = b
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

// TakeAll atomically retrieves and removes a batch of images by ID,
// verifying ownership for each. Semantics:
//
//   - If EVERY id resolves (present + unexpired + owned by `owner`), all
//     entries are removed in a single critical section and the images
//     are returned in `ids` order. The returned slice is nil when ids
//     is empty.
//   - If ANY id fails any check, NOTHING is removed and (nil, err) is
//     returned. `err` carries the same "not found or expired" semantics
//     as the single-id path so callers can keep a single client-facing
//     error message regardless of which specific id broke.
//
// This closes R37-CONCUR4: the legacy "Take in a loop" pattern would
// consume N-1 entries before hitting the broken N-th id, and the
// already-consumed entries would be silently dropped on the error
// return — user loses the image data with no recovery hook. With
// TakeAll the invariant is "all-or-nothing", so a partial-expiry burst
// surfaces as a single error and the caller can prompt the user to
// re-upload all images in one shot.
func (s *uploadStore) TakeAll(ids []string, owner string) ([]cli.ImageData, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// First pass: validate every id under the single lock so expiry
	// eviction in the second pass cannot invalidate a peer mid-walk.
	// Collect entry pointers keyed by position to avoid a second map
	// lookup when consuming.
	resolved := make([]*uploadEntry, len(ids))
	now := time.Now()
	for i, id := range ids {
		e, ok := s.entries[id]
		if !ok {
			return nil, errUploadNotFound
		}
		if now.Sub(e.Created) > uploadTTL {
			// Expired entries are removed eagerly in this same lock so a
			// caller that retries with fresh uploads doesn't keep tripping
			// over the same stale id — but we do NOT touch the other ids'
			// entries that ARE still valid. This preserves "all-or-nothing"
			// for the current batch while cleaning house opportunistically.
			s.removeEntryLocked(id, e)
			return nil, errUploadNotFound
		}
		if e.Owner != owner {
			return nil, errUploadNotFound
		}
		resolved[i] = e
	}

	// All ids validated — now consume them in the same lock.
	out := make([]cli.ImageData, len(ids))
	for i, id := range ids {
		out[i] = resolved[i].Image
		s.removeEntryLocked(id, resolved[i])
	}
	return out, nil
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
