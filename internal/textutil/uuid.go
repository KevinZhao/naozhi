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

// DashlessUUID strips dashes from a Claude-style UUID so its shape matches
// the event log's dashless 32-char hex identities; "" for empty or non-hex
// input.
func DashlessUUID(u string) string {
	if u == "" {
		return ""
	}
	// Stack buffer avoids a per-row alloc on JSONL replay.
	var b [32]byte
	n := 0
	for i := 0; i < len(u); i++ {
		c := u[i]
		if c == '-' {
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return ""
		}
		if n >= 32 {
			return ""
		}
		if c >= 'A' && c <= 'F' {
			c = c + 32
		}
		b[n] = c
		n++
	}
	if n != 32 {
		return ""
	}
	return string(b[:])
}
