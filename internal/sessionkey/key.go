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
	// PlannerNSPrefix is the namespace of the one global Planner
	// (docs/rfc/planner.md §15.1), unrelated to the project planners above.
	PlannerNSPrefix = "planner:"
	// TaskKeyPrefix is the namespace of the sessions the Planner dispatches,
	// "task:<random ID>" (§3.2): the key says nothing else; purpose,
	// lineage and workspace live in the sidecar file.
	TaskKeyPrefix = "task:"
	// ScoutKeyPrefix is the namespace of the one-shot scout sessions,
	// "scout:<run ID>" (docs/rfc/planner.md §15.6).
	ScoutKeyPrefix = "scout:"
	// MeetingKeyPrefix is the namespace of the one-shot meeting summary
	// sessions, "meeting:<meeting ID>" (docs/rfc/meeting-notes.md §4).
	MeetingKeyPrefix = "meeting:"
)

// PlannerMainKey is the global Planner's session key.
const PlannerMainKey = PlannerNSPrefix + "main"

// DashboardPlatform is the platform segment (parts[0]) of dashboard-
// originated session keys.
const DashboardPlatform = "dashboard"

// DashboardProjectChatType is the chatType segment (parts[1]) of a
// project-level stable dashboard session key, shaped
// `dashboard:pj:<workspace-hash>:<agent>` (see ProjectStableKey).
// "pj" rather than "project" so no segment collides with the planner
// namespace (`project:...`), keeping the two unambiguous.
const DashboardProjectChatType = "pj"

// PlannerKeySuffix is the trailing token that distinguishes a planner key
// from any future `project:{name}:<role>` sub-role.
const PlannerKeySuffix = ":planner"

// CronKey returns the canonical router key "cron:<jobID>" (jobID is a
// 16-char hex from cron.generateHexID).
func CronKey(jobID string) string { return CronKeyPrefix + jobID }

// IsCronKey reports whether s belongs to the cron namespace.
func IsCronKey(s string) bool { return strings.HasPrefix(s, CronKeyPrefix) }

// IsSysKey reports whether s belongs to the system-session namespace.
func IsSysKey(s string) bool { return strings.HasPrefix(s, SysKeyPrefix) }

// IsPlannerNSKey reports whether s belongs to the global Planner's namespace.
func IsPlannerNSKey(s string) bool { return strings.HasPrefix(s, PlannerNSPrefix) }

// IsTaskKey reports whether s belongs to the Planner's dispatched sessions.
func IsTaskKey(s string) bool { return strings.HasPrefix(s, TaskKeyPrefix) }

// IsScoutKey reports whether s belongs to the scout sessions' namespace.
func IsScoutKey(s string) bool { return strings.HasPrefix(s, ScoutKeyPrefix) }

// IsMeetingKey reports whether s belongs to the meeting summary sessions.
func IsMeetingKey(s string) bool { return strings.HasPrefix(s, MeetingKeyPrefix) }

// IsOneShotKey reports whether s is a background session that lives for one
// turn and is reset after it (scout runs, meeting summaries): never
// persisted, listed, sent to, or watched by health.
func IsOneShotKey(s string) bool { return IsScoutKey(s) || IsMeetingKey(s) }

// IsScratchKey reports whether s belongs to the dashboard scratch namespace.
func IsScratchKey(s string) bool { return strings.HasPrefix(s, ScratchKeyPrefix) }

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
