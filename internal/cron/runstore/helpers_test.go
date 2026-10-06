package runstore

import (
	"crypto/rand"
	"encoding/hex"
)

// mustGenerateID / mustGenerateRunID mint the 16-hex IDs cron assigns to jobs
// and runs, panicking on a crypto/rand failure that cannot happen in a test
// process.
func mustGenerateID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("runstore test: rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func mustGenerateRunID() string { return mustGenerateID() }
