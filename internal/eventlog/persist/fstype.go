package persist

// FileSystem classification for the event-log directory. Surfaced on
// /health.eventlog.fs_type and in doctor output so operators can tell
// at a glance whether their storage is supported. See RFC §5.4.
//
// Values are deliberately short lowercase identifiers — they are
// rendered directly into JSON without translation.
const (
	FSTypeExt4    = "ext4"
	FSTypeXFS     = "xfs"
	FSTypeAPFS    = "apfs"
	FSTypeTmpfs   = "tmpfs"
	FSTypeNFS     = "nfs"
	FSTypeOverlay = "overlayfs"
	FSTypeBtrfs   = "btrfs"
	FSTypeFUSE    = "fuse"
	FSTypeUnknown = "unknown"
)

// FSDetection is the result of DetectFS. Kept as a dedicated struct
// so callers can surface both the type label and a boolean support
// signal without re-implementing the "is this safe?" decision.
type FSDetection struct {
	// Type is the short classification label (one of the FSType*
	// constants). "unknown" means detection succeeded at the syscall
	// layer but the returned type code did not match any known
	// entry in fsTypeMap — may happen on exotic filesystems or
	// platforms we haven't catalogued.
	Type string

	// Supported reports whether the detected filesystem is one we
	// claim to write event logs reliably on. Used by /health and
	// doctor to render a warning banner.
	Supported bool

	// Err is set when the detection syscall itself failed (directory
	// does not exist, permission denied, non-Linux platform where
	// Statfs is not implemented). A non-nil Err does NOT mean the
	// filesystem is unsupported — callers should treat it as
	// "unknown, degrade gracefully".
	Err error
}
