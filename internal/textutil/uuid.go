package textutil

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// DeriveLegacyUUID computes a stable UUID for a legacy event entry that
// does not have an UUID field set. Used by the Claude CLI JSONL reader
// (internal/discovery) and the historical replay path so that two
// separate ingests of the same Claude message map onto the same event
// identity, allowing MergedSource to dedup them against newer naozhi-
// native records.
//
// The derivation is intentionally deterministic and stable across naozhi
// versions — changing the hash input would break dedup for any entry
// produced by an older naozhi. The inputs we hash are:
//
//	"v1" | timestamp(unix ms) | type | summary | detail
//
// Time + summary is usually enough to identify an event uniquely within a
// session; detail is folded in for tool_use / result events where summary
// is a repetitive one-liner ("Bash", "Read") but detail carries the actual
// command / output.
//
// The "v1" prefix guards against accidentally migrating to a new
// derivation rule without bumping — any change here MUST flip the prefix
// to "v2" and carry a CHANGELOG note that MergedSource's dedup window is
// effectively reset.
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
