package persist

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// keyHashBytes is the SHA-256 prefix length used in file names: 16 bytes →
// 32 hex chars, ~2^-64 collision probability.
const keyHashBytes = 16

// File suffixes under events/. All relatives share the stem
// hex(SHA-256(key)[:keyHashBytes]) so one Glob matches them all.
const (
	// logExt is the append-only framed record file.
	logExt = ".log"
	// idxExt is the fixed-width sparse index sidecar (see schema/idx.go).
	idxExt = ".idx"
	// tmpInfix marks rotate staging files: "<stem>.tmp.<epoch>.log";
	// orphans are deleted on startup.
	tmpInfix = ".tmp."
)

// KeyHash derives the file-name stem: lowercase hex of the first
// keyHashBytes bytes of SHA-256(key). One-way, so listings do not reveal
// session identities; the plaintext key lives in schema.FileHeader.Key
// inside <stem>.log.
func KeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:keyHashBytes])
}

// LogPath returns the full path to the <stem>.log file for a key under dir.
func LogPath(dir, key string) string {
	return filepath.Join(dir, KeyHash(key)+logExt)
}

// tmpLogPath / tmpIdxPath return rotate-staging paths
// "<stem>.tmp.<epoch>.{log,idx}". Rotate is serialized on the writer
// goroutine; the epoch only keeps test-fabricated staging files apart.
func tmpLogPath(dir, stem string, epoch int64) string {
	return filepath.Join(dir, stem+tmpInfix+itoa(epoch)+logExt)
}

func tmpIdxPath(dir, stem string, epoch int64) string {
	return filepath.Join(dir, stem+tmpInfix+itoa(epoch)+idxExt)
}

// IsLogFileName reports whether base is a committed <stem>.log (not a
// tmp-rotate staging file).
func IsLogFileName(base string) bool {
	if !strings.HasSuffix(base, logExt) {
		return false
	}
	stem := strings.TrimSuffix(base, logExt)
	if strings.Contains(stem, tmpInfix) {
		return false
	}
	return isHexStem(stem)
}

// IsIdxFileName is the idx counterpart — symmetric logic for sweep.
func IsIdxFileName(base string) bool {
	if !strings.HasSuffix(base, idxExt) {
		return false
	}
	stem := strings.TrimSuffix(base, idxExt)
	if strings.Contains(stem, tmpInfix) {
		return false
	}
	return isHexStem(stem)
}

// IsTmpFileName reports whether base is a rotate-staging file; only a
// completed rotate commits via rename, so startup removes any such file.
func IsTmpFileName(base string) bool {
	if !strings.Contains(base, tmpInfix) {
		return false
	}
	return strings.HasSuffix(base, logExt) || strings.HasSuffix(base, idxExt)
}

// isHexStem verifies s is exactly keyHashBytes*2 lowercase hex chars;
// anything else (operator noise, a future naming scheme) is left alone.
func isHexStem(s string) bool {
	if len(s) != keyHashBytes*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// itoa avoids pulling strconv into the file-path helpers; epochs are
// always positive.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
