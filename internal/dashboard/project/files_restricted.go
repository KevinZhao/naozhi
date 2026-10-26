package project

import (
	"os"
	"path/filepath"
	"strings"
)

// processEUID is the naozhi process EUID, captured once so
// isRestrictedForeignPrivate stays syscall-free and tests can override it.
var processEUID = uint32(os.Geteuid())

// isRestrictedForeignPrivate refuses files under a restricted root
// (include_root) that are owner-private (no group/world bits) AND owned by a
// UID other than the naozhi process (#831). Linux DAC checks the running
// process, not the dashboard caller, so without this gate a dashboard user
// could read another OS user's 0600 files. World/group-readable and same-UID files stay accessible. Reads the
// already-Lstat'd FileInfo, so zero syscalls on the hot path.
func isRestrictedForeignPrivate(info os.FileInfo) bool {
	uid, ok := fileOwnerUID(info)
	if !ok {
		// Cannot read the owner UID (non-Unix or stub FileInfo): fail closed.
		// Production is always Linux where ok is true.
		return true
	}
	if uid == processEUID {
		return false
	}
	const groupOrWorld = 0o077
	return info.Mode().Perm()&groupOrWorld == 0
}

// restrictedDeniedSuffixes lists basename suffixes never served through a
// restricted root even when world/group readable (#1330): a workspace can hold
// world-accessible Unix sockets (ssh-agent, gpg-agent, postgres/redis IPC)
// whose payload is authentication state, .pid files aid kill/ptrace probes,
// and core/crash dumps are memory snapshots. Matched on the basename of the
// resolved path so a directory component called "ssh" does not trip it.
var restrictedDeniedSuffixes = []string{
	".sock",
	".pid",
}

// restrictedDeniedSubstrings catches names without a known suffix
// (`ssh-agent.<pid>`, `S.gpg-agent`, the `.xauthority` MIT-MAGIC-COOKIE file,
// `.dbus-keyrings`); case-insensitive on the basename.
var restrictedDeniedSubstrings = []string{
	"ssh",
	"gpg",
	".xauthority",
	".dbus",
}

// restrictedDeniedPrefixes catches dump/crash artefacts whose names start
// with a known marker followed by a pid/timestamp. Matched on the
// case-insensitive basename so `core.1234` and `crash.txt` both trip.
var restrictedDeniedPrefixes = []string{
	"core.",
	"crash.",
}

// isRestrictedDeniedName reports whether the basename of resolved is refused by
// a restricted root regardless of mode bits (see restrictedDeniedSuffixes).
func isRestrictedDeniedName(resolved string) bool {
	name := strings.ToLower(filepath.Base(resolved))
	if name == "" || name == "." || name == "/" {
		return false
	}
	for _, suf := range restrictedDeniedSuffixes {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	for _, sub := range restrictedDeniedSubstrings {
		if strings.Contains(name, sub) {
			return true
		}
	}
	for _, pre := range restrictedDeniedPrefixes {
		if strings.HasPrefix(name, pre) {
			return true
		}
	}
	return false
}

// isRestrictedIrregularType reports whether the file is a Unix socket, FIFO or
// device node, none of which may be served through a restricted root (#1688). A
// world-readable socket with an unlisted name passes both the deny-list and
// the foreign-private gate, yet reflecting it discloses IPC payload; FIFOs and
// devices can block or leak kernel state. Independent of name and permission
// bits; zero syscalls (uses the Lstat'd FileInfo).
func isRestrictedIrregularType(info os.FileInfo) bool {
	const irregular = os.ModeSocket | os.ModeNamedPipe | os.ModeDevice | os.ModeCharDevice
	return info.Mode()&irregular != 0
}
