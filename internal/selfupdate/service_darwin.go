//go:build darwin

package selfupdate

import (
	"os"
	"path/filepath"
)

// LaunchdPlistPath returns the LaunchAgents plist path for the naozhi service.
func LaunchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist")
}

func launchdPlistPath() string { return LaunchdPlistPath() }
