//go:build darwin

package persist

import (
	"strings"
	"syscall"
)

// DetectFS on macOS reads Statfs_t.Fstypename (NUL-padded []int8) rather
// than a numeric code. APFS and hfs are treated as supported; durability on
// macOS (dev-only target) is best effort anyway.
func DetectFS(dir string) FSDetection {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return FSDetection{
			Type:      FSTypeUnknown,
			Supported: false,
			Err:       err,
		}
	}
	// Fstypename is [16]int8 NUL-padded.
	buf := make([]byte, 0, len(s.Fstypename))
	for _, c := range s.Fstypename {
		if c == 0 {
			break
		}
		buf = append(buf, byte(c))
	}
	name := strings.ToLower(string(buf))
	switch name {
	case "apfs":
		return FSDetection{Type: FSTypeAPFS, Supported: true}
	case "hfs":
		// APFS-equivalent for supportedness.
		return FSDetection{Type: FSTypeAPFS, Supported: true}
	case "nfs":
		return FSDetection{Type: FSTypeNFS, Supported: false}
	case "tmpfs":
		return FSDetection{Type: FSTypeTmpfs, Supported: false}
	case "osxfuse", "macfuse", "fuse":
		return FSDetection{Type: FSTypeFUSE, Supported: false}
	default:
		return FSDetection{Type: FSTypeUnknown, Supported: false}
	}
}
