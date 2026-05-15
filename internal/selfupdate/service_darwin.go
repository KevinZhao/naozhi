//go:build darwin

package selfupdate

import (
	"os"
	"path/filepath"
)

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}
