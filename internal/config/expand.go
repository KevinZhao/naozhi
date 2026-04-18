package config

import "github.com/naozhi/naozhi/internal/osutil"

// ExpandHome replaces a leading ~/ with the user's home directory.
// Deprecated: use osutil.ExpandHome directly.
func ExpandHome(path string) string {
	return osutil.ExpandHome(path)
}
