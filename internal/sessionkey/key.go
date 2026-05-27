// Package sessionkey owns the canonical key prefixes used to namespace
// router sessions across subsystems (cron / sys / scratch).
//
// Why a dedicated leaf package: the prefixes were previously defined in
// internal/session and re-used by internal/cron / internal/sysession via
// session.IsCronKey / session.IsSysKey / session.IsScratchKey. That
// forced cron and sysession to import session just for these constants,
// which contributed to the cron → session reverse import cycle the
// cron-sysession-merge RFC closes. Moving the prefix vocabulary to a
// dedicated leaf lets every subsystem reference the same constants
// without depending on each other.
//
// Invariant: this package MUST NOT import any other internal/* package.
// Enforced by depguard in .golangci.yml — a future change that adds an
// internal import here breaks lint.
package sessionkey

import "strings"

// Prefix constants. Each is the literal substring written at the start
// of a router session key to identify which subsystem owns the session.
//
// Wire-stable: these strings appear in dashboard WS subscriptions, in
// session.Router lookups, in cron_jobs.json cron stub keys, etc.
// Renaming requires a coordinated migration across the entire codebase.
const (
	CronKeyPrefix    = "cron:"
	SysKeyPrefix     = "sys:"
	ScratchKeyPrefix = "scratch:"
)

// CronKey returns the canonical router key for a cron job ID.
// Format: "cron:<jobID>" — jobID typically a 16-char hex from
// cron.generateHexID.
func CronKey(jobID string) string { return CronKeyPrefix + jobID }

// SysKey returns the canonical router key for a system-session daemon ID.
// Format: "sys:<daemonID>".
func SysKey(daemonID string) string { return SysKeyPrefix + daemonID }

// ScratchKey returns the canonical router key for a dashboard scratch
// (follow-up drawer) session ID. Format: "scratch:<sessionID>".
func ScratchKey(sessionID string) string { return ScratchKeyPrefix + sessionID }

// IsCronKey reports whether s belongs to the cron namespace.
func IsCronKey(s string) bool { return strings.HasPrefix(s, CronKeyPrefix) }

// IsSysKey reports whether s belongs to the system-session namespace.
func IsSysKey(s string) bool { return strings.HasPrefix(s, SysKeyPrefix) }

// IsScratchKey reports whether s belongs to the dashboard scratch namespace.
func IsScratchKey(s string) bool { return strings.HasPrefix(s, ScratchKeyPrefix) }

// CronJobIDFromKey returns the trailing job ID of a cron key, or the empty
// string when s is not a cron key. Convenience for the common pattern
//
//	if IsCronKey(s) { jobID := s[len(CronKeyPrefix):] }
//
// where the conditional + slice arithmetic gets duplicated across handlers.
func CronJobIDFromKey(s string) string {
	if !IsCronKey(s) {
		return ""
	}
	return s[len(CronKeyPrefix):]
}
