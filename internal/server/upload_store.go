package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

const (
	uploadTTL         = 10 * time.Minute
	uploadCleanupFreq = 1 * time.Minute
	maxUploadEntries  = 100 // global cap to prevent memory exhaustion
	// maxUploadPerOwner caps live entries per owner so one forgotten tab
	// cannot starve every other user with 429 until the next cleanup tick.
	maxUploadPerOwner = 40
	// maxFilesPerSend caps files (inline + pre-uploaded) per send on the WS,
	// HTTP JSON and multipart paths. Kept below maxUploadPerOwner so a full
	// batch plus an in-flight retry fits the per-owner quota.
	maxFilesPerSend = 20
	// errTooManyFiles is the user-facing error for maxFilesPerSend; the build
	// assertion below pins its literal to the constant.
	errTooManyFiles = "too many files (max 20)"
	// maxUploadBytesPerOwner is the real per-owner memory rail (PDFs are up to
	// 32 MB each, so the entry-count cap alone would allow ~1.28 GB resident).
	maxUploadBytesPerOwner = 96 * 1024 * 1024 // 96 MB
	// maxUploadBytesGlobal caps total live payload so colluding owners cannot
	// starve the host through the per-owner cap.
	maxUploadBytesGlobal = 512 * 1024 * 1024 // 512 MB
)

// _ pins errTooManyFiles literal "20" to maxFilesPerSend at compile time.
var _ = [1]struct{}{}[20-maxFilesPerSend]

type uploadEntry struct {
	Image   cli.Attachment
	Owner   string
	Created time.Time
}

// uploadStore holds pre-uploaded images keyed by random ID.
// Entries expire after uploadTTL and are cleaned up periodically.
//
// Invariants (all under mu): ownerCounts[o] == |{e | e.Owner==o}|,
// ownerBytes[o] == Σ entrySize over that owner's entries, totalBytes == Σ over
// all entries. Empty owner is stored under unknownOwner (see Put).
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
	// missing, expired, or owned by someone else. The fid is user-supplied
	// and MUST NOT be echoed back to the client.
	errUploadNotFound = errors.New("file not found or expired")
)

// unknownOwner is the per-owner bucket key for an empty owner string, so
// empty-owner callers (no token + unresolvable clientIP) still fall under a
// per-owner quota instead of bypassing it and saturating the global cap.
const unknownOwner = "__unknown__"

// Put stores an image owned by owner and returns a random hex ID.
// Returns errUploadStoreFull when either the global entry cap or global
// byte cap is hit, or errUploadPerOwner when the caller's entry/byte
// sub-limit would be exceeded. A crypto/rand failure is mapped to a
// transient errUploadStoreFull (plus slog.Error) rather than a panic.
func (s *uploadStore) Put(owner string, img cli.Attachment) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		slog.Error("uploadStore Put: crypto/rand unavailable", "err", err)
		return "", errUploadStoreFull
	}
	id := hex.EncodeToString(b)

	sz := entrySize(img)

	bucket := owner
	if bucket == "" {
		bucket = unknownOwner
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= maxUploadEntries {
		return "", errUploadStoreFull
	}
	if s.totalBytes+sz > maxUploadBytesGlobal {
		return "", errUploadStoreFull
	}
	if s.ownerCounts[bucket] >= maxUploadPerOwner {
		return "", errUploadPerOwner
	}
	if s.ownerBytes[bucket]+sz > maxUploadBytesPerOwner {
		return "", errUploadPerOwner
	}
	s.entries[id] = &uploadEntry{Image: img, Owner: bucket, Created: time.Now()}
	s.totalBytes += sz
	s.ownerCounts[bucket]++
	s.ownerBytes[bucket] += sz
	return id, nil
}

// entrySize reports the payload byte count used for quota accounting; only
// Data contributes.
func entrySize(img cli.Attachment) int64 {
	return int64(len(img.Data))
}

// removeEntryLocked decrements the owner counter / byte accounting and
// deletes the entry. Caller must hold s.mu.
func (s *uploadStore) removeEntryLocked(id string, e *uploadEntry) {
	// Idempotence guard: a duplicate id in a TakeAll batch resolves to the
	// same entry twice and would double-decrement the accounting (#2335).
	if cur, ok := s.entries[id]; !ok || cur != e {
		return
	}
	delete(s.entries, id)
	sz := entrySize(e.Image)
	s.totalBytes -= sz
	if s.totalBytes < 0 {
		s.totalBytes = 0
	}
	// Defensive empty→unknownOwner fold; local var so the entry isn't mutated.
	owner := e.Owner
	if owner == "" {
		owner = unknownOwner
	}
	if n := s.ownerCounts[owner] - 1; n <= 0 {
		// n < 0 means an unbalanced Put/Take pair; log rather than mask it.
		if n < 0 {
			slog.Warn("upload store: ownerCounts underflow, resetting to zero", "owner", owner)
		}
		delete(s.ownerCounts, owner)
	} else {
		s.ownerCounts[owner] = n
	}
	if b := s.ownerBytes[owner] - sz; b <= 0 {
		if b < 0 {
			slog.Warn("upload store: ownerBytes underflow, resetting to zero", "owner", owner)
		}
		delete(s.ownerBytes, owner)
	} else {
		s.ownerBytes[owner] = b
	}
}

// Take retrieves and removes an image by ID, verifying ownership.
// Returns nil if not found, expired, or owner does not match — callers
// receive the same "not found" response regardless of the failure reason
// to avoid leaking the existence of another user's upload.
func (s *uploadStore) Take(id, owner string) *cli.Attachment {
	if owner == "" {
		owner = unknownOwner
	}
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
// verifying ownership for each. All-or-nothing: if every id resolves
// (present + unexpired + owned), all are removed in one critical section and
// returned in `ids` order (nil for empty ids); if ANY fails, nothing valid is
// removed and errUploadNotFound is returned, so a partial-expiry burst never
// silently consumes the caller's other images.
func (s *uploadStore) TakeAll(ids []string, owner string) ([]cli.Attachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if owner == "" {
		owner = unknownOwner
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// First pass validates every id; nothing valid is removed until all pass.
	resolved := make([]*uploadEntry, len(ids))
	now := time.Now()
	for i, id := range ids {
		e, ok := s.entries[id]
		if !ok {
			return nil, errUploadNotFound
		}
		if now.Sub(e.Created) > uploadTTL {
			// Evict the expired entry only; still-valid peers stay untouched.
			s.removeEntryLocked(id, e)
			return nil, errUploadNotFound
		}
		if e.Owner != owner {
			return nil, errUploadNotFound
		}
		resolved[i] = e
	}

	out := make([]cli.Attachment, len(ids))
	for i, id := range ids {
		out[i] = resolved[i].Image
		s.removeEntryLocked(id, resolved[i])
	}
	return out, nil
}

// Peek returns a COPY of the image owned by `owner` under `id` WITHOUT
// removing it. Returns nil on not-found / expired / wrong-owner with the same
// opacity as Take. Data is copied so a caller mutating it cannot corrupt the
// stored entry between Peek and Replace.
func (s *uploadStore) Peek(id, owner string) *cli.Attachment {
	if owner == "" {
		owner = unknownOwner
	}
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
	cp := e.Image
	cp.Data = append([]byte(nil), e.Image.Data...)
	return &cp
}

// Replace overwrites the Data/MimeType of an existing live entry in place,
// preserving id, owner and Created (the TTL is not extended). The new size is
// re-checked against the per-owner and global byte caps. Returns false on
// not-found / expired / wrong-owner / would-exceed-cap; the caller then keeps
// the original bytes.
func (s *uploadStore) Replace(id, owner string, img cli.Attachment) bool {
	if owner == "" {
		owner = unknownOwner
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return false
	}
	if time.Since(e.Created) > uploadTTL {
		s.removeEntryLocked(id, e)
		return false
	}
	if e.Owner != owner {
		return false
	}
	oldSz := entrySize(e.Image)
	newSz := entrySize(img)
	delta := newSz - oldSz
	if delta > 0 {
		if s.totalBytes+delta > maxUploadBytesGlobal {
			return false
		}
		if s.ownerBytes[owner]+delta > maxUploadBytesPerOwner {
			return false
		}
	}
	e.Image = img
	s.totalBytes += delta
	if s.totalBytes < 0 {
		s.totalBytes = 0
	}
	if b := s.ownerBytes[owner] + delta; b <= 0 {
		delete(s.ownerBytes, owner)
	} else {
		s.ownerBytes[owner] = b
	}
	return true
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
