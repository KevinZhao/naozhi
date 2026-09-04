// Webhook signature / timestamp verification for Feishu encrypt-key mode.
package feishu

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"time"
)

// Webhook timestamp freshness window, asymmetric on purpose: 5 min in the
// past covers latency and Feishu retries; only 30 s in the future tolerates
// clock skew without a wide pre-issuance window for nonce-replay amplification.
const (
	webhookTimestampFutureSkew = 30
	webhookTimestampMaxAge     = 5 * 60
)

// verifySignature verifies the encrypt_key-mode signature
// SHA256(ts+nonce+key+body) via incremental hashing (no 64 KB concat) and a
// constant-time compare. An empty key returns false — never "skip": callers
// MUST gate on encryptKey != "" themselves so the config check stays auditable.
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

// verifyTimestamp checks that the request timestamp is within the window above.
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
