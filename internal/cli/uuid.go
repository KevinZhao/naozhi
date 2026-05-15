package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
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
func newEventUUID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Defensive fallback: hash a monotonic nanosecond counter so
		// at least UUIDs remain unique within one process even in an
		// error scenario. The returned bytes are still 16 long so
		// the schema shape doesn't shift.
		sum := sha256.Sum256([]byte("naozhi-crypto-rand-fallback-" + strconv.FormatInt(int64(uuidFallbackSeq.Add(1)), 10)))
		copy(buf[:], sum[:])
	}
	return hex.EncodeToString(buf[:])
}

// uuidFallbackSeq is the monotonic counter the crypto/rand fallback
// path reads. Package-scoped so it survives across goroutines in the
// (extremely unlikely) event that multiple fallback allocations
// happen in quick succession.
var uuidFallbackSeq atomic.Int64
