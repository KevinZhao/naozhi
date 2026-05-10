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

// DeriveLegacyUUID computes a stable UUID for a legacy EventEntry that
// does not have an UUID field set. Used by the Claude CLI JSONL reader
// (internal/discovery) and the historical replay path so that two
// separate ingests of the same Claude message map onto the same
// event identity, allowing MergedSource to dedup them against newer
// naozhi-native records.
//
// The derivation is intentionally deterministic and stable across
// naozhi versions — changing the hash input would break dedup for
// any entry produced by an older naozhi. The inputs we hash are:
//
//	"v1" | timestamp(unix ms) | type | summary | detail
//
// Time + summary is usually enough to identify an event uniquely
// within a session; detail is folded in for tool_use / result events
// where summary is a repetitive one-liner ("Bash", "Read") but
// detail carries the actual command / output.
//
// The "v1" prefix guards against accidentally migrating to a new
// derivation rule without bumping — any change here MUST flip the
// prefix to "v2" and carry a CHANGELOG note that MergedSource's
// dedup window is effectively reset.
func DeriveLegacyUUID(timeMS int64, typ, summary, detail string) string {
	h := sha256.New()
	h.Write([]byte("v1\x00"))
	h.Write([]byte(strconv.FormatInt(timeMS, 10)))
	h.Write([]byte{0x00})
	h.Write([]byte(typ))
	h.Write([]byte{0x00})
	h.Write([]byte(summary))
	h.Write([]byte{0x00})
	h.Write([]byte(detail))
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

// uuidFallbackSeq is the monotonic counter the crypto/rand fallback
// path reads. Package-scoped so it survives across goroutines in the
// (extremely unlikely) event that multiple fallback allocations
// happen in quick succession.
var uuidFallbackSeq atomic.Int64
