package server

import (
	"crypto/sha256"
	"encoding/hex"
)

// bearerOwner mirrors uploadOwner's Bearer branch: sha256(token)[:16] hex.
// Keeping the upload owner deterministic lets the test Put a file under the
// exact bucket handleSend will resolve from the same Bearer header.
func bearerOwner(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}
