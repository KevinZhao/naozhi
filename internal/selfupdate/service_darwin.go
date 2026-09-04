//go:build darwin

package selfupdate

import (
	"os"
	"path/filepath"
)

// LaunchdPlistPath returns the LaunchAgents plist path for `naozhi install`.
// Built from the LaunchdLabel CONSTANT — right for install, not for addressing
// a running job (see launchdServiceLabel / verifiedLaunchdLabel).
func LaunchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist")
}
