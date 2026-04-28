package discovery

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ProcStartTime returns a value that uniquely identifies a process instance
// even after PID reuse. On Darwin we use ps(1) to get the process start time
// and encode it as Unix microseconds.
//
// Unix microseconds reach MaxSafeJSONInt (2^53-1 ≈ 9.00e15) only near the
// year 2255 (current Unix μs ≈ 1.77e15), so JS front-ends (dashboard.js) can
// safely consume the field via JSON.parse without double-precision
// truncation. See MaxSafeJSONInt in scanner.go; proc_darwin_test.go pins
// the invariant. If the encoding is ever changed (e.g. to nanoseconds), the
// budget collapses — replace μs×1000 and re-check against the guard test.
func ProcStartTime(pid int) (uint64, error) {
	// ps -o lstart= outputs e.g. "Sat Apr 12 14:30:00 2026"
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, fmt.Errorf("ps lstart for pid %d: %w", pid, err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, fmt.Errorf("ps returned empty lstart for pid %d", pid)
	}
	// ps(1) on Darwin prints lstart in the local timezone without a zone
	// suffix; Parse (no location) would interpret the wallclock as UTC and
	// shift start-time identity by the UTC offset (e.g. +8h in Shanghai),
	// breaking stale-shim detection after TZ-sensitive restarts.
	t, err := time.ParseInLocation("Mon Jan  2 15:04:05 2006", s, time.Local)
	if err != nil {
		// Fallback: try single-digit day format "Mon Jan 2 15:04:05 2006"
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

// detectCLIName uses ps(1) to determine which CLI binary is running.
// Returns "claude-code", "kiro", or "cli" as fallback.
func detectCLIName(pid int) string {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "cli"
	}
	cmd := strings.TrimSpace(string(out))
	if i := strings.IndexByte(cmd, ' '); i >= 0 {
		cmd = cmd[:i]
	}
	bin := filepath.Base(cmd)
	switch {
	case strings.Contains(bin, "kiro"):
		return "kiro"
	case strings.Contains(bin, "claude"):
		return "claude-code"
	default:
		return "cli"
	}
}
