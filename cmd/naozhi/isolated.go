package main

import (
	"log/slog"
	"os"
	"os/user"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/config"
)

// isolatedInstance reports whether stateRoot (the session store's directory)
// is not ~/.naozhi in the account's own home: a test instance started with a
// temporary HOME or store_path. The account's home comes from the user
// database, not $HOME, so a temporary HOME counts too. An unknown account
// home counts as not isolated.
func isolatedInstance(stateRoot string) bool {
	if stateRoot == "" {
		return false
	}
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return false
	}
	own := filepath.Join(u.HomeDir, ".naozhi")
	if filepath.Clean(stateRoot) == own {
		return false
	}
	// Both may name one directory by different spellings (a symlink, the
	// case macOS folds); the directory not existing yet is not the own one.
	a, errA := os.Stat(stateRoot)
	b, errB := os.Stat(own)
	return errA != nil || errB != nil || !os.SameFile(a, b)
}

// schedulesOff reports whether this instance skips the scheduled jobs that
// read outside sources and the update installs (config.IsolationConfig),
// logging what it skips once.
func schedulesOff(cfg *config.Config, stateRoot string) bool {
	if !isolatedInstance(stateRoot) || cfg.Isolation.RunSchedules {
		return false
	}
	slog.Warn("isolated instance: no scout runs, pre-meeting briefs or scheduled cron runs, updates only notified; isolation.run_schedules: true runs them",
		"state_root", stateRoot)
	return true
}
