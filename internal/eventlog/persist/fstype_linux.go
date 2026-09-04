//go:build linux

package persist

import (
	"syscall"
)

// Linux filesystem magic numbers from /usr/include/linux/magic.h (`man 2
// statfs`); hex literals kept as in the header so "0xef53 ext4" greps here.
const (
	magicExt4    = 0xef53
	magicXFS     = 0x58465342
	magicBtrfs   = 0x9123683e
	magicTmpfs   = 0x01021994
	magicNFS     = 0x6969
	magicOverlay = 0x794c7630
	// fuseblk (older kernels) shares this code.
	magicFUSE = 0x65735546
)

// DetectFS classifies the filesystem hosting dir for /health.eventlog.fs_type
// (RFC §5.4). Statfs is ~1μs and deliberately uncached so admin-time mount
// changes show up.
func DetectFS(dir string) FSDetection {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return FSDetection{
			Type:      FSTypeUnknown,
			Supported: false,
			Err:       err,
		}
	}
	// Statfs_t.Type is int32 on some arches and int64 on others; cast
	// through uint64 so the comparisons are platform-agnostic.
	code := uint64(s.Type)
	switch code {
	case magicExt4:
		return FSDetection{Type: FSTypeExt4, Supported: true}
	case magicXFS:
		return FSDetection{Type: FSTypeXFS, Supported: true}
	case magicBtrfs:
		// Supported; the runbook documents the COW caveats.
		return FSDetection{Type: FSTypeBtrfs, Supported: true}
	case magicTmpfs:
		// Ephemeral by design; exposed so operators do not mistake it for durable.
		return FSDetection{Type: FSTypeTmpfs, Supported: false}
	case magicNFS:
		return FSDetection{Type: FSTypeNFS, Supported: false}
	case magicOverlay:
		return FSDetection{Type: FSTypeOverlay, Supported: false}
	case magicFUSE:
		return FSDetection{Type: FSTypeFUSE, Supported: false}
	default:
		return FSDetection{Type: FSTypeUnknown, Supported: false}
	}
}
