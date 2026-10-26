package ring

import (
	"crypto/rand"
	"encoding/hex"
)

// newEventUUID returns a fresh 16-byte crypto/rand identity encoded as 32
// lowercase hex characters; EventLog.Append stamps it on every entry that
// arrives without a UUID, making UUID the identity key for persistence and
// merge dedup (collision odds are negligible at the ring/persist sizes).
// crypto/rand.Read never returns an error (it aborts the process on an OS
// failure), so no fallback is needed.
func newEventUUID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}
