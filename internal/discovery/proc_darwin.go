//go:build darwin

package discovery

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// psPath pins ps(1) to /bin/ps so a PATH-manipulation attack (a malicious
// "ps" earlier in PATH) cannot replace the lookup; /bin is always on the
// macOS root filesystem.
const psPath = "/bin/ps"

// ProcStartTime returns a value that uniquely identifies a process instance
// even after PID reuse: the ps(1) start time as Unix microseconds, which stay
// below MaxSafeJSONInt until ~2255 so dashboard.js can JSON.parse the field
// without truncation (proc_darwin_test.go pins this).
func ProcStartTime(pid int) (uint64, error) {
	// ps -o lstart= outputs e.g. "Sat Apr 12 14:30:00 2026"
	out, err := exec.Command(psPath, "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, fmt.Errorf("ps lstart for pid %d: %w", pid, err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, fmt.Errorf("ps returned empty lstart for pid %d", pid)
	}
	// Darwin ps prints lstart in local time with no zone suffix; parsing as
	// UTC would shift the start-time identity by the UTC offset and break
	// stale-shim detection after TZ-sensitive restarts.
	t, err := time.ParseInLocation("Mon Jan  2 15:04:05 2006", s, time.Local)
	if err != nil {
		t, err = time.ParseInLocation("Mon Jan 2 15:04:05 2006", s, time.Local)
		if err != nil {
			return 0, fmt.Errorf("parse lstart %q for pid %d: %w", s, pid, err)
		}
	}
	usec := uint64(t.Unix())*1_000_000 + uint64(t.Nanosecond()/1000)
	if usec == 0 {
		usec = 1 // ensure non-zero
	}
	return usec, nil
}

func procPidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
func procKillSIGKILL(pid int)   { _ = syscall.Kill(pid, syscall.SIGKILL) }

// detectCLIName uses ps(1) to determine which CLI binary is running: the
// first registered backend.Profile whose DetectInProc matches the binary
// basename wins. See docs/rfc/multi-backend.md §3.4.
func detectCLIName(pid int) string {
	out, err := exec.Command(psPath, "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "cli"
	}
	cmd := strings.TrimSpace(string(out))
	if i := strings.IndexByte(cmd, ' '); i >= 0 {
		cmd = cmd[:i]
	}
	bin := filepath.Base(cmd)
	for _, p := range backend.All() {
		if p.DetectInProc != nil && p.DetectInProc(bin) {
			return p.DisplayName
		}
	}
	return "cli"
}
