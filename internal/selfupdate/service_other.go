//go:build !darwin

package selfupdate

// LaunchdPlistPath returns empty on non-darwin platforms.
func LaunchdPlistPath() string { return "" }
