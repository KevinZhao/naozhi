package sandboxstore

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
)

// newTestStore returns a Store rooted at a fresh temp dir.
func newTestStore(t *testing.T) Store {
	t.Helper()
	return Store{Root: t.TempDir()}
}

// hexID returns a random 16-char scheduler-shaped id.
func hexID(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}
