package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"
	"sync/atomic"
)

// newEventUUID returns a fresh 16-byte crypto/rand identity encoded
// as 32 lowercase hex characters. Used by EventLog.Append on every
// entry that arrives without a UUID set, making UUID the authoritative
// identity key for downstream persistence and merge dedup.
//
// Collision probability: at 2^-64 per pair, a naozhi process would
// need to write ~4 billion events before even a 1% chance of a
// collision. The live system caps EventLog ring at 500 entries, and
// on-disk persist caps per-session files at ~100 MiB (well under
// 2^32 records total), so collisions are not a practical concern.
//
// crypto/rand never returns short reads on Linux; an error here is
// a sign of a hard OS failure. We fall back to a deterministic-ish
// hash-of-seed identity rather than panicking — the Append call
// came from an event hot path and must not crash the whole server.
//
// R233B-PERF-7: high-frequency event ingest (50 sessions × 50 evt/s =
// 2500 events/s) makes the 16-byte getrandom syscall a measurable
// fraction of Append cost. We refill 256-byte goroutine-local pools
// per call so steady-state Append pulls from a slice rather than the
// kernel — amortising the syscall cost down by 16×.
func newEventUUID() string {
	var raw [16]byte
	pulled := pullFromUUIDPool(raw[:])
	if !pulled {
		if _, err := rand.Read(raw[:]); err != nil {
			// Defensive fallback: hash a monotonic nanosecond counter so
			// at least UUIDs remain unique within one process even in an
			// error scenario. The returned bytes are still 16 long so
			// the schema shape doesn't shift.
			sum := sha256.Sum256([]byte("naozhi-crypto-rand-fallback-" + strconv.FormatInt(int64(uuidFallbackSeq.Add(1)), 10)))
			copy(raw[:], sum[:])
		}
	}
	// hex.Encode into a stack-resident array avoids the intermediate
	// []byte allocation that hex.EncodeToString performs internally.
	var dst [32]byte
	hex.Encode(dst[:], raw[:])
	return string(dst[:])
}

// uuidPoolBytes is the per-bucket refill size. 256 bytes = 16 UUIDs;
// large enough to amortise the syscall, small enough that an idle
// pool returned via sync.Pool's GC reclaim path doesn't waste much.
const uuidPoolBytes = 256

// uuidPool buckets pre-fetched random bytes per goroutine. Each
// bucket carries a 256-byte buffer + cursor. When the cursor reaches
// the end, the next pull triggers a refill (one rand.Read per 16
// uuids amortised).
type uuidBucket struct {
	buf [uuidPoolBytes]byte
	pos int
}

var uuidPool = sync.Pool{
	New: func() any { return &uuidBucket{pos: uuidPoolBytes} },
}

// pullFromUUIDPool fills dst (16 bytes) from a goroutine-local random
// bucket and returns true on success. Returns false when the bucket
// refill itself fails — caller falls back to a direct rand.Read.
func pullFromUUIDPool(dst []byte) bool {
	b := uuidPool.Get().(*uuidBucket)
	defer uuidPool.Put(b)
	if b.pos+16 > uuidPoolBytes {
		if _, err := rand.Read(b.buf[:]); err != nil {
			b.pos = uuidPoolBytes // force next pull to retry
			return false
		}
		b.pos = 0
	}
	copy(dst, b.buf[b.pos:b.pos+16])
	// Zero the consumed slot defensively — pooled buckets may live
	// across multiple goroutines via sync.Pool's GC churn, so we don't
	// want a freshly-handed-out bucket to expose another goroutine's
	// already-issued UUIDs to a debugger.
	for i := b.pos; i < b.pos+16; i++ {
		b.buf[i] = 0
	}
	b.pos += 16
	return true
}

// uuidFallbackSeq is the monotonic counter the crypto/rand fallback
// path reads. Package-scoped so it survives across goroutines in the
// (extremely unlikely) event that multiple fallback allocations
// happen in quick succession.
var uuidFallbackSeq atomic.Int64
