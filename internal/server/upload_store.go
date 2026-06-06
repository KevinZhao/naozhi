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
	// errTooManyFiles is the user-facing error returned by every code path
	// that tripped maxFilesPerSend. Pre-built as a compile-time constant so
	// the hot error path skips fmt.Sprintf reflection (R224-PERF-5). The
	// build assertion below this const block pins this literal to
	// maxFilesPerSend; flip the cap without updating the message and the
	// build fails.
	errTooManyFiles = "too many files (max 20)"
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

// _ pins errTooManyFiles literal "20" to maxFilesPerSend at compile time.
// If maxFilesPerSend changes without updating the literal in errTooManyFiles
// (and its three call sites' tests), this assertion fails to compile.
var _ = [1]struct{}{}[20-maxFilesPerSend]

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

// unknownOwner is used as the per-owner bucket key when Put is called
// with an empty owner string. Without this fallback, empty-owner callers
// (no dashboard token configured + unresolvable clientIP) would bypass
// per-owner quotas entirely and could saturate the global cap for every
// other user — effectively a DoS vector against shared slot capacity.
// Attackers who can spoof IPs (trustedProxy=false) still share this one
// bucket, preserving a cap rather than removing it.
const unknownOwner = "__unknown__"

// Put stores an image owned by owner and returns a random hex ID.
// Returns errUploadStoreFull when either the global entry cap or global
// byte cap is hit, or errUploadPerOwner when the caller's entry/byte
// sub-limit would be exceeded.
//
// R247-SEC-20: crypto/rand failure used to panic, taking the entire HTTP
// server down. The kernel RNG initialisation can hiccup early in boot on
// containers that share /dev/urandom under heavy IO; map that to a
// transient errUploadStoreFull (callers already retry on this sentinel
// with a "try again later" prompt) plus a slog.Error so operators can
// page on the underlying RNG outage rather than discover it via a process
// crash.
func (s *uploadStore) Put(owner string, img cli.ImageData) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		slog.Error("uploadStore Put: crypto/rand unavailable", "err", err)
		return "", errUploadStoreFull
	}
	id := hex.EncodeToString(b)

	sz := entrySize(img)

	// Empty-owner callers fold into a single shared bucket so they still
	// participate in per-owner accounting. Previously empty owner bypassed
	// the sub-limit entirely, leaving global quota as the only cap.
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
	// Defensive empty→unknownOwner fold in case a future refactor
	// bypasses Put's owner normalisation; matches Take/TakeAll semantics.
	// Use a local variable so we don't mutate the caller's *uploadEntry.
	owner := e.Owner
	if owner == "" {
		owner = unknownOwner
	}
	if n := s.ownerCounts[owner] - 1; n <= 0 {
		// R228-GO-P3-6: an underflow (n < 0) means a Put/Take pair is
		// unbalanced — log so the bug surfaces during operations rather
		// than silently masking the accounting drift, mirroring the
		// totalBytes defensive log above.
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
func (s *uploadStore) Take(id, owner string) *cli.ImageData {
	// Mirror Put's empty-owner fold so Take can match entries stored
	// under the shared unknownOwner bucket.
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
	// Mirror Put's empty-owner fold so batched Takes match entries stored
	// under the shared unknownOwner bucket.
	if owner == "" {
		owner = unknownOwner
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

// Peek returns a COPY of the image owned by `owner` under `id` WITHOUT
// removing it — the entry stays live for the eventual send. Returns nil on
// not-found / expired / wrong-owner, with the same opacity as Take so the
// existence of another user's upload never leaks. Used by the auto-orient
// endpoint, which needs to read the bytes, decide a rotation, and write the
// corrected bytes back via Replace while the user keeps the file pending.
//
// The returned ImageData carries a fresh copy of the Data slice so a caller
// mutating it can't corrupt the stored entry under the lock-free window
// between Peek and Replace.
func (s *uploadStore) Peek(id, owner string) *cli.ImageData {
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
// preserving its id, owner, and Created timestamp (so auto-orient does NOT
// extend or reset the entry's TTL). The byte-accounting deltas are applied
// under the same lock, and the post-replace size is re-checked against the
// per-owner and global byte caps — a rotation can only change JPEG size
// modestly, but we never let it push an owner over quota. Returns false on
// not-found / expired / wrong-owner / would-exceed-cap; the caller then
// keeps the original (unrotated) bytes, which is the correct fail-safe.
func (s *uploadStore) Replace(id, owner string, img cli.ImageData) bool {
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
		// Only a growing payload can break a cap; a shrink always fits.
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
