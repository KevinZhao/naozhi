// Package sessionkey owns the canonical key prefixes that namespace
// router sessions across subsystems (cron / sys / scratch / project).
//
// Invariant: this package MUST NOT import any other internal/* package
// (enforced by depguard in .golangci.yml), so every subsystem can share it.
package sessionkey

import "strings"

// Prefix constants: the literal substring at the start of a router session
// key identifying the owning subsystem. Wire-stable: they appear in dashboard
// WS subscriptions, session.Router lookups and cron_jobs.json stub keys.
const (
	CronKeyPrefix    = "cron:"
	SysKeyPrefix     = "sys:"
	ScratchKeyPrefix = "scratch:"
	// ProjectKeyPrefix is the namespace prefix for project-scoped session
	// keys; the canonical key shape is `project:{name}:planner`.
	ProjectKeyPrefix = "project:"
)

// DashboardPlatform is the platform segment (parts[0]) of dashboard-
// originated session keys.
const DashboardPlatform = "dashboard"

// DashboardProjectChatType is the chatType segment (parts[1]) of a
// project-level stable dashboard session key, shaped
// `dashboard:pj:<workspace-hash>:<agent>` (see internal/session.ProjectStableKey).
// "pj" rather than "project" so no segment collides with the planner
// namespace (`project:...`), keeping the two unambiguous.
const DashboardProjectChatType = "pj"

// PlannerKeySuffix is the trailing token that distinguishes a planner key
// from any future `project:{name}:<role>` sub-role.
const PlannerKeySuffix = ":planner"

// CronKey returns the canonical router key "cron:<jobID>" (jobID is a
// 16-char hex from cron.generateHexID).
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

// IsDashboardProjectKey reports whether key is a project-level stable
// dashboard session key (`dashboard:pj:<id>...`). Pure prefix scan, no
// allocation; false for planner (`project:...`), scratch, cron and sys keys.
func IsDashboardProjectKey(key string) bool {
	const prefix = DashboardPlatform + ":" + DashboardProjectChatType + ":"
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	// A bare "dashboard:pj:" (missing workspace hash) is not accepted.
	return len(key) > len(prefix)
}

// CronJobIDFromKey returns the trailing job ID of a cron key, or the empty
// string when s is not a cron key.
func CronJobIDFromKey(s string) string {
	if !IsCronKey(s) {
		return ""
	}
	return s[len(CronKeyPrefix):]
}

// PlannerKeyFor returns the canonical planner session key
// `project:{name}:planner`. Callers must have validated `name` against the
// project name regex (internal/project.ValidateProjectName); sessionkey
// performs no validation so it can stay zero-dep.
func PlannerKeyFor(name string) string {
	return ProjectKeyPrefix + name + PlannerKeySuffix
}

// IsPlannerKey reports whether key is a planner session key. Returns false
// for the empty-name edge case (`project::planner`) and any key missing
// the prefix or suffix.
func IsPlannerKey(key string) bool {
	if !strings.HasPrefix(key, ProjectKeyPrefix) {
		return false
	}
	if !strings.HasSuffix(key, PlannerKeySuffix) {
		return false
	}
	// The {name} segment must be non-empty to identify a real project.
	return len(key) > len(ProjectKeyPrefix)+len(PlannerKeySuffix)
}

// PlannerNameFromKey extracts {name} from a planner key. Returns the empty
// string for any non-planner input (the IsPlannerKey gate makes the
// function total instead of panicking on too-short input).
func PlannerNameFromKey(key string) string {
	if !IsPlannerKey(key) {
		return ""
	}
	return key[len(ProjectKeyPrefix) : len(key)-len(PlannerKeySuffix)]
}
