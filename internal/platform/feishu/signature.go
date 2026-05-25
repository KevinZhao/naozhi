// Webhook crypto verification for Feishu encrypt-key mode. Extracted from
// feishu.go (R214-ARCH-13 minimal split) so the signature/timestamp gate
// reads as a self-contained 60-line block instead of being buried at line
// ~1268 of a 1459-line file.
//
// No behavior change: the two functions and the two timestamp-window
// constants moved verbatim. Callers in transport_hook.go and the existing
// table tests in feishu_test.go continue to work because the package
// surface (function names, signatures, package-level constants) is
// preserved.
package feishu

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"time"
)

// Webhook timestamp freshness window. Asymmetric on purpose:
//
//   - webhookTimestampMaxAge (5 min in the past) covers normal network
//     latency and legitimate Feishu-side retries. R218-SEC-13.
//   - webhookTimestampFutureSkew (30 s in the future) tolerates clock
//     skew without giving attackers a wide pre-issuance window for
//     nonce-replay amplification. R218-SEC-13.
const (
	webhookTimestampFutureSkew = 30
	webhookTimestampMaxAge     = 5 * 60
)

// verifySignature verifies the request signature (for encrypt_key mode).
// Uses the incremental hash.Hash interface to avoid copying the body into a
// concatenated string — webhook bodies can be up to 64 KB, and the old
// `timestamp + nonce + encryptKey + string(body)` path allocated ~64 KB per
// request and did it twice (once for the string, once for the []byte cast).
// Also hex-encodes via encoding/hex to avoid the fmt.Sprintf "%x" parse
// overhead, and compares as bytes under ConstantTimeCompare without stringy
// intermediate allocation.
//
// R224-SEC-2: callers MUST gate this call on `encryptKey != ""` themselves.
// The earlier "empty key → return true" internal fallback was a footgun:
// any future caller forgetting the outer guard would silently bypass
// signature verification entirely. Empty key now returns false (a missing
// signature cannot be valid), forcing the configuration check to live at
// the call site where it's auditable.
func verifySignature(timestamp, nonce, encryptKey string, body []byte, signature string) bool {
	if encryptKey == "" {
		return false
	}
	h := sha256.New()
	h.Write([]byte(timestamp))
	h.Write([]byte(nonce))
	h.Write([]byte(encryptKey))
	h.Write(body)
	var sumBuf [sha256.Size]byte
	sum := h.Sum(sumBuf[:0])
	var hexBuf [sha256.Size * 2]byte
	hex.Encode(hexBuf[:], sum)
	return subtle.ConstantTimeCompare(hexBuf[:], []byte(signature)) == 1
}

// verifyTimestamp checks that the request timestamp is plausibly recent.
// See the webhookTimestamp* constants above for the window rationale.
func verifyTimestamp(timestamp string) bool {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	now := time.Now().Unix()
	if ts > now+webhookTimestampFutureSkew {
		return false
	}
	if now-ts > webhookTimestampMaxAge {
		return false
	}
	return true
}
