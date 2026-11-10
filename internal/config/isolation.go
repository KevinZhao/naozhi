package config

// IsolationConfig is for an isolated instance: one whose state root is not
// ~/.naozhi in the account's own home (a temporary HOME or store_path). Such
// an instance runs no scheduled job that reads an outside source — the scout
// and its pre-meeting briefs, the cron jobs' scheduled runs — and never
// installs an update, so a test instance started from a copy of the real
// config neither reads the user's mail nor replaces the binary it shares.
// A manual cron run still goes.
type IsolationConfig struct {
	// RunSchedules runs them anyway, as on the account's own instance.
	RunSchedules bool `yaml:"run_schedules,omitempty"`
}
