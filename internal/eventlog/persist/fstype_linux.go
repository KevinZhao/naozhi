//go:build linux

package persist

import (
	"syscall"
)

// Linux filesystem magic numbers. Sourced from
// /usr/include/linux/magic.h and confirmed against `man 2 statfs`.
// Hex literals kept in the constants (not computed) so the source
// reads like the header — operators grepping for "0xef53 ext4" find
// it here.
//
// The macOS build (fstype_darwin.go) does not read these; they are
// Linux-kernel values.
const (
	magicExt4    = 0xef53
	magicXFS     = 0x58465342
	magicBtrfs   = 0x9123683e
	magicTmpfs   = 0x01021994
	magicNFS     = 0x6969
	magicOverlay = 0x794c7630
	// FUSE on modern Linux kernels reports a single magic; fuseblk
	// (older 3.x kernels) shares the same code, so only one constant
	// is needed.
	magicFUSE = 0x65735546
)

// DetectFS classifies the filesystem hosting dir. The classification
// drives /health.eventlog.fs_type and the supportedness banner
// described in RFC §5.4.
//
// The syscall is cheap (~1μs) so we run it once per NewPersister
// and once per /health request that needs it. No caching here —
// caching would hide admin-time mount changes.
func DetectFS(dir string) FSDetection {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return FSDetection{
			Type:      FSTypeUnknown,
			Supported: false,
			Err:       err,
		}
	}
	// Statfs_t.Type is int32 on some arches and int64 on others;
	// cast through uint64 so the comparisons below are
	// platform-agnostic.
	code := uint64(s.Type)
	switch code {
	case magicExt4:
		return FSDetection{Type: FSTypeExt4, Supported: true}
	case magicXFS:
		return FSDetection{Type: FSTypeXFS, Supported: true}
	case magicBtrfs:
		// Btrfs supports fsync properly on modern kernels; list as
		// supported but we document "use with COW caveats" in the
		// runbook.
		return FSDetection{Type: FSTypeBtrfs, Supported: true}
	case magicTmpfs:
		// tmpfs loses data on reboot by design; we expose the fact
		// so operators don't mistake ephemeral storage for durable.
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
