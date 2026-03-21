package pathutil

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandHome replaces a leading ~/ with the user's home directory.
func ExpandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
