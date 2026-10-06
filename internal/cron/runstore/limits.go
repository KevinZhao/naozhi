package runstore

import (
	"time"

	"github.com/naozhi/naozhi/internal/textutil"
)

// Retention defaults — fallbacks when Options leaves KeepCount / KeepWindow
// zero. A run is kept only when (count_rank ≤ keepCount) AND
// (age ≤ keepWindow); either condition false → trim.
const (
	DefaultKeepCount  = 200
	DefaultKeepWindow = 30 * 24 * time.Hour
)

// MaxRecordBytes caps a single CronRun JSON payload. It is a per-record format
// invariant, not operator-tunable: old record files may exist on disk above a
// lowered cap. Cron's 4K-rune Result + 512-rune ErrorMsg + 8K Prompt + ~512
// metadata add up to ~13 KiB worst case; 32 KiB leaves headroom. Reading a
// file larger than this returns ErrCorruptRun.
const MaxRecordBytes = 32 * 1024

// TruncatedSuffix marks where TruncateWithSuffix cut a string that exceeded
// its rune budget, in run records and in cron_jobs.json alike.
const TruncatedSuffix = "…[truncated]"

// TruncateWithSuffix returns s rune-truncated to maxRunes, appending
// TruncatedSuffix only when the input was actually shrunk. Idempotent on
// already-clean strings.
func TruncateWithSuffix(s string, maxRunes int) string {
	trimmed := textutil.TruncateRunesNoEllipsis(s, maxRunes)
	if len(trimmed) >= len(s) {
		return s
	}
	return trimmed + TruncatedSuffix
}

// ValidID reports whether s is a valid cron job or run identifier: a non-empty
// lowercase hex string of at most 64 bytes. IDs become directory and file
// names under runs/, so every store entry point refuses anything else before
// touching disk; uppercase hex, path characters and temp/backup suffixes are
// all rejected, which also filters stray files out of a scan.
func ValidID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
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
