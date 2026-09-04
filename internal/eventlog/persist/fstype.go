package persist

// Filesystem classification labels for the event-log directory, surfaced on
// /health.eventlog.fs_type and in doctor output (RFC §5.4). Short lowercase
// identifiers rendered directly into JSON.
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

// FSDetection is the result of DetectFS: the type label plus a support
// signal so callers need not re-derive "is this safe?".
type FSDetection struct {
	// Type is one of the FSType* labels; "unknown" means the syscall
	// succeeded but the code is not catalogued.
	Type string

	// Supported reports whether event logs are claimed reliable on this
	// filesystem; /health and doctor render a warning banner when false.
	Supported bool

	// Err is set when the detection syscall itself failed (missing dir,
	// permission denied, platform without Statfs). Non-nil Err means
	// "unknown, degrade gracefully", not "unsupported".
	Err error
}
