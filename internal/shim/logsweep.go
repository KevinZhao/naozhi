package shim

import (
	"strconv"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
)

// logsweep.go — the liveness half of the shims/ retention pass (J6 of #2548).
// The naming half is Run() in server.go, which writes shim-<pid>.log next to the
// state file; keeping the parser here means the two cannot drift apart.

// LogFilePrefix is the shim log-file name prefix. Exported so the retention pass
// and Run() agree on one spelling.
const LogFilePrefix = "shim-"

// LogFileIsLive reports whether name is a shim log whose process still exists,
// i.e. whether a retention pass must keep it. It is deliberately conservative:
// a name this function cannot parse, and a pid that still resolves, are both
// kept. A recycled pid therefore preserves a dead shim's log rather than
// deleting a live one's.
//
// Suitable as datadir.Pass.Keep, which is only consulted for files already past
// the pass's MaxAge — so a shim that just crashed keeps its log for the grace
// period, which is when it is worth reading.
func LogFileIsLive(name string) bool {
	if !strings.HasPrefix(name, LogFilePrefix) || !strings.HasSuffix(name, ".log") {
		return true // not ours; never sweep it
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, LogFilePrefix), ".log")
	// Only a bare run of digits, and only a positive value: os.Getpid() never
	// yields 0 or a sign, so "shim-0.log" / "shim--1.log" / "shim-+7.log" did not
	// come from Run() and are not this pass's to remove. strconv.Atoi alone would
	// accept all three and hand them to PidAlive, which reports non-positive pids
	// as dead — sweeping a file we do not own.
	if digits == "" || strings.TrimLeft(digits, "0123456789") != "" {
		return true
	}
	pid, err := strconv.Atoi(digits)
	if err != nil || pid <= 0 {
		return true // out of int range, or all zeroes — leave it alone
	}
	// A syntactically valid pid that no longer resolves is dead whatever its
	// magnitude, so an absurdly large one is still sweepable.
	return osutil.PidAlive(pid)
}
