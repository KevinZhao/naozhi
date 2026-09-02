//go:build darwin

package selfupdate

import (
	"os"
	"path/filepath"
)

// LaunchdPlistPath returns the LaunchAgents plist path for the naozhi service.
//
// Consumed by cmd/naozhi's install/uninstall, which writes and removes the
// plist. Nothing inside this package needs it any more: restartLaunchd used to
// `unload`+`load` the file, and `kickstart -k` addresses the job by label
// instead (RFC §3.6b), so the package-private wrapper that used to sit here went
// away with it.
//
// Note it is built from the LaunchdLabel CONSTANT, which is right for install
// (that is the label naozhi would write) but not for talking to a job that is
// already running — see launchdServiceLabel/verifiedLaunchdLabel for that.
func LaunchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist")
}
