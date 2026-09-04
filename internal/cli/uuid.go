package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"
	"sync/atomic"
)

// newEventUUID returns a fresh 16-byte crypto/rand identity encoded as 32
// lowercase hex characters; EventLog.Append stamps it on every entry that
// arrives without a UUID, making UUID the identity key for persistence and
// merge dedup (collision odds are negligible at the ring/persist sizes).
// Random bytes come from a pooled bucket so steady-state Append avoids a
// getrandom syscall per event. crypto/rand never short-reads on Linux; an
// error is a hard OS failure and we fall back to a hashed counter rather than
// panic on the event hot path.
func newEventUUID() string {
	var raw [16]byte
	pulled := pullFromUUIDPool(raw[:])
	if !pulled {
		if _, err := rand.Read(raw[:]); err != nil {
			// Hash a monotonic counter so UUIDs stay unique in-process and the
			// 16-byte shape is preserved.
			sum := sha256.Sum256([]byte("naozhi-crypto-rand-fallback-" + strconv.FormatInt(int64(uuidFallbackSeq.Add(1)), 10)))
			copy(raw[:], sum[:])
		}
	}
	// Stack array avoids hex.EncodeToString's intermediate allocation.
	var dst [32]byte
	hex.Encode(dst[:], raw[:])
	return string(dst[:])
}

// uuidPoolBytes is the per-bucket refill size: 4096 bytes = 256 UUIDs per
// rand.Read. Memory is bounded at one bucket per active goroutine (sync.Pool
// reclaims idle ones); larger buckets give diminishing returns and hold stale
// randomness longer.
const uuidPoolBytes = 4096

// uuidBucket is a pooled buffer of pre-fetched random bytes plus a cursor;
// a pull past the end triggers a refill.
type uuidBucket struct {
	buf [uuidPoolBytes]byte
	pos int
}

var uuidPool = sync.Pool{
	New: func() any { return &uuidBucket{pos: uuidPoolBytes} },
}

// pullFromUUIDPool fills dst (16 bytes) from a pooled random bucket and
// returns false when the refill fails (caller falls back to rand.Read).
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
	// Zero the consumed slot so a bucket handed to another goroutine never
	// exposes already-issued UUIDs.
	for i := b.pos; i < b.pos+16; i++ {
		b.buf[i] = 0
	}
	b.pos += 16
	return true
}

// uuidFallbackSeq is the monotonic counter the crypto/rand fallback path
// reads (shared with newSlotUUID).
var uuidFallbackSeq atomic.Int64
