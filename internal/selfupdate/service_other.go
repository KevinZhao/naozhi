//go:build !darwin

package selfupdate

// LaunchdPlistPath returns empty on non-darwin platforms.
//
// Still exported for cmd/naozhi's install/uninstall, which writes and removes
// the plist. The package-private wrapper that used to sit beside it went away
// with restartLaunchd's `unload`+`load` implementation: `kickstart -k` addresses
// the job by label, so nothing inside this package needs a plist path any more.
func LaunchdPlistPath() string { return "" }
