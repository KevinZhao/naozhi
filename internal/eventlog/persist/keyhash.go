package persist

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// keyHashBytes is the number of SHA-256 prefix bytes used in file
// names. 16 bytes → 32 hex chars → 2^-64 collision probability for up
// to billions of distinct keys. More bytes would just widen file
// names without material safety gain.
const keyHashBytes = 16

// suffixes and prefixes we plant under the events/ directory. They
// share a common stem = hex(SHA-256(key)[:keyHashBytes]) so one DropKey
// can match all relatives via filepath.Glob in a single call.
const (
	// logExt is the append-only framed record file.
	logExt = ".log"
	// idxExt is the fixed-width sparse index sidecar (see schema/idx.go).
	idxExt = ".idx"
	// tmpInfix appears in the tmp rotate staging path to distinguish
	// in-progress rotates from regular files: "<stem>.tmp.<epoch>.log".
	// We detect and delete any orphaned tmp file on startup.
	tmpInfix = ".tmp."
)

// KeyHash derives a stem for file names from a session key. The hash
// is one-way (sha256 prefix) so operators reading file listings can
// NOT infer session identities by eyeballing; they must cross-reference
// the file's header record (schema.FileHeader.Key) which is written
// in plaintext inside <stem>.log.
//
// The stem is hex-encoded (32 lowercase chars) so it is:
//   - safe across all filesystems (no escape required)
//   - deterministic (re-hashing the same key always yields the same stem)
//   - cheap to compare (no runtime conversions needed)
//
// The sum is truncated to keyHashBytes bytes. Truncation is intentional
// — shorter file names beat the vanishingly small collision risk from
// the full 32-byte hash.
func KeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:keyHashBytes])
}

// LogPath returns the full path to the <stem>.log file for a key under dir.
func LogPath(dir, key string) string {
	return filepath.Join(dir, KeyHash(key)+logExt)
}

// IdxPath returns the full path to the <stem>.idx file for a key under dir.
func IdxPath(dir, key string) string {
	return filepath.Join(dir, KeyHash(key)+idxExt)
}

// tmpLogPath / tmpIdxPath return rotate-staging paths for a given stem
// and epoch. Epoch is usually time.Now().UnixNano() to disambiguate
// racing rotate attempts (rotate is serialized through the single
// writer goroutine, so races don't actually occur in production, but
// the epoch keeps tests that fabricate multiple staging files from
// stomping on each other).
func tmpLogPath(dir, stem string, epoch int64) string {
	return filepath.Join(dir, stem+tmpInfix+itoa(epoch)+logExt)
}

func tmpIdxPath(dir, stem string, epoch int64) string {
	return filepath.Join(dir, stem+tmpInfix+itoa(epoch)+idxExt)
}

// IsLogFileName reports whether base is a <stem>.log file (not a
// tmp-rotate staging file). Used by the orphan sweep to avoid mistaking
// a half-rotated file for a committed session log.
func IsLogFileName(base string) bool {
	if !strings.HasSuffix(base, logExt) {
		return false
	}
	// Reject "<stem>.tmp.<epoch>.log" — tmp files are swept separately.
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

// IsTmpFileName reports whether base is a rotate-staging file. Startup
// cleanup removes any such file because only a completed rotate has
// its `rename()` commit the new log/idx atomically.
func IsTmpFileName(base string) bool {
	if !strings.Contains(base, tmpInfix) {
		return false
	}
	return strings.HasSuffix(base, logExt) || strings.HasSuffix(base, idxExt)
}

// isHexStem verifies the bare stem (no extension, no tmp infix) is
// exactly keyHashBytes*2 lowercase hex chars. Anything else is either
// operator noise or a naming scheme from a future naozhi version;
// either way, we leave it alone.
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

// itoa is a mini int64-to-string used to avoid pulling strconv into
// the hot-path file helpers. The epoch values here are always positive
// (time.Now().UnixNano()), so signedness handling is trivial.
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
