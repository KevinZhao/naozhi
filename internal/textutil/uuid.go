package textutil

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// DeriveLegacyUUID computes a stable UUID for a legacy event entry without a
// UUID field, so two ingests of the same Claude message (JSONL reader and
// replay) map onto one identity and MergedSource can dedup them. Hash input:
//
//	"v1" | timestamp(unix ms) | type | summary | detail
//
// The rule (field set, order, separators, digest) MUST stay stable across
// naozhi versions; any change must flip the "v1" prefix to "v2". A call site
// correcting the values it passes is a bug fix, not a rule change — derived
// UUIDs are recomputed per LoadBefore and never persisted (#2445).
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
