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
	// ProjectKeyPrefix is the namespace prefix for project-scoped session
	// keys. The canonical key shape is `project:{name}:planner`. R040034-ARCH-2
	// (#1412): consolidated from the former internal/keyspec leaf package — it
	// shared this namespace concept with internal/sessionkey but lived as a
	// parallel zero-dep leaf, splitting the "session-key vocabulary" across
	// two indistinguishable packages. A single owner is now the source of
	// truth for cron / sys / scratch / project prefixes.
	ProjectKeyPrefix = "project:"
)

// PlannerKeySuffix is the trailing token that distinguishes a planner
// key from any future `project:{name}:<role>` sub-roles. Today every
// `project:` key is a planner key, but the constant exists so adding
// a new role (e.g. `project:foo:tasks`) does not require rewriting
// the suffix-match logic in two places. R040034-ARCH-2 (#1412):
// migrated from internal/keyspec.
const PlannerKeySuffix = ":planner"

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

// PlannerKeyFor returns the canonical planner session key for the
// given project name. Callers must have validated `name` against the
// project name regex (see internal/project.ValidateProjectName) —
// sessionkey performs no validation so it can stay zero-dep.
//
// Format: `project:{name}:planner`. R040034-ARCH-2 (#1412): migrated
// from internal/keyspec. Migration of existing literals must continue
// to satisfy:
//
//	PlannerKeyFor("foo") == "project:foo:planner"
//
// which is asserted by both internal/project and internal/session
// format-locked tests.
func PlannerKeyFor(name string) string {
	return ProjectKeyPrefix + name + PlannerKeySuffix
}

// IsPlannerKey reports whether the given key is a planner session
// key. Returns false for both the empty-name edge case
// (`project::planner`) and any key missing the prefix or suffix.
//
// The empty-name rejection mirrors the pre-extraction logic so that
// any caller migrating to sessionkey sees identical behaviour.
// R040034-ARCH-2 (#1412): migrated from internal/keyspec.
func IsPlannerKey(key string) bool {
	if !strings.HasPrefix(key, ProjectKeyPrefix) {
		return false
	}
	if !strings.HasSuffix(key, PlannerKeySuffix) {
		return false
	}
	// Reject the boundary case "project::planner" — the {name} segment
	// must be non-empty for the key to identify a real project.
	return len(key) > len(ProjectKeyPrefix)+len(PlannerKeySuffix)
}

// PlannerNameFromKey extracts {name} from a planner key. Returns the
// empty string for any non-planner input (including keys shorter than
// prefix+suffix, missing prefix/suffix, or the empty-name edge case
// `project::planner`).
//
// R20260526-CR-009: previously the function sliced unconditionally and
// would panic on `slice bounds out of range` when given a too-short
// input. The godoc told callers to gate on IsPlannerKey first, but a
// silent caller bug would crash with an uninformative runtime panic.
// The self-defense IsPlannerKey gate makes the function total: well-
// formed callers pay one extra HasPrefix+HasSuffix check (cheap), ill-
// formed callers get a typed empty-string instead of a panic.
// R040034-ARCH-2 (#1412): migrated from internal/keyspec.
func PlannerNameFromKey(key string) string {
	if !IsPlannerKey(key) {
		return ""
	}
	return key[len(ProjectKeyPrefix) : len(key)-len(PlannerKeySuffix)]
}
