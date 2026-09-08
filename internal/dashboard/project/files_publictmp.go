package project

import (
	"os"
	"path/filepath"
	"strings"
)

// processEUID is the naozhi process EUID, captured once so
// isPublicTmpForeignPrivate stays syscall-free and tests can override it.
var processEUID = uint32(os.Geteuid())

// isPublicTmpForeignPrivate refuses /tmp files that are owner-private (no
// group/world bits) AND owned by a UID other than the naozhi process (#831).
// Linux DAC checks the running process, not the dashboard caller, so without
// this gate a dashboard user could read another OS user's 0600 files under
// /tmp. World/group-readable and same-UID files stay accessible. Reads the
// already-Lstat'd FileInfo, so zero syscalls on the hot path.
func isPublicTmpForeignPrivate(info os.FileInfo) bool {
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

// publicTmpDeniedSuffixes lists basename suffixes never served through
// __public_tmp__ even when world/group readable (#1330): /tmp holds
// world-accessible Unix sockets (ssh-agent, gpg-agent, postgres/redis IPC)
// whose payload is authentication state, .pid files aid kill/ptrace probes,
// and core/crash dumps are memory snapshots. Matched on the basename of the
// resolved path so a directory component called "ssh" does not trip it.
var publicTmpDeniedSuffixes = []string{
	".sock",
	".pid",
}

// publicTmpDeniedSubstrings catches names without a known suffix
// (`ssh-agent.<pid>`, `S.gpg-agent`, the `.xauthority` MIT-MAGIC-COOKIE file,
// `.dbus-keyrings`); case-insensitive on the basename.
var publicTmpDeniedSubstrings = []string{
	"ssh",
	"gpg",
	".xauthority",
	".dbus",
}

// publicTmpDeniedPrefixes catches dump/crash artefacts whose names start
// with a known marker followed by a pid/timestamp. Matched on the
// case-insensitive basename so `core.1234` and `crash.txt` both trip.
var publicTmpDeniedPrefixes = []string{
	"core.",
	"crash.",
}

// isPublicTmpDeniedName reports whether the basename of resolved is refused by
// __public_tmp__ regardless of mode bits (see publicTmpDeniedSuffixes).
func isPublicTmpDeniedName(resolved string) bool {
	name := strings.ToLower(filepath.Base(resolved))
	if name == "" || name == "." || name == "/" {
		return false
	}
	for _, suf := range publicTmpDeniedSuffixes {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	for _, sub := range publicTmpDeniedSubstrings {
		if strings.Contains(name, sub) {
			return true
		}
	}
	for _, pre := range publicTmpDeniedPrefixes {
		if strings.HasPrefix(name, pre) {
			return true
		}
	}
	return false
}

// isPublicTmpIrregularType reports whether the file is a Unix socket, FIFO or
// device node, none of which may be served through __public_tmp__ (#1688). A
// world-readable socket with an unlisted name passes both the deny-list and
// the foreign-private gate, yet reflecting it discloses IPC payload; FIFOs and
// devices can block or leak kernel state. Independent of name and permission
// bits; zero syscalls (uses the Lstat'd FileInfo).
func isPublicTmpIrregularType(info os.FileInfo) bool {
	const irregular = os.ModeSocket | os.ModeNamedPipe | os.ModeDevice | os.ModeCharDevice
	return info.Mode()&irregular != 0
}
