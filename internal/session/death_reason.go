package session

// The router's own death reasons, next to cli's process-level DeathReason*
// constants (internal/cli/process.go): a process the router reclaimed on
// purpose, rather than one that ended on its own. Exported so contractjs can
// enumerate the full death_reason wire vocabulary without restating these two
// as string literals (#2909 G5 PR2).
const (
	// DeathReasonIdleTimeout marks a session the router reclaimed for being
	// idle past its timeout.
	DeathReasonIdleTimeout = "idle_timeout"
	// DeathReasonEvicted marks a session the router reclaimed to free
	// capacity for a newer one.
	DeathReasonEvicted = "evicted"
)
