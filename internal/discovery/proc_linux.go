//go:build linux

package discovery

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// procPidPath builds "/proc/<pid>/<leaf>" without going through fmt.Sprintf,
// which on the dashboard scan path would allocate one format buffer + one
// final string per PID. Bytes are written into a small stack-friendly buffer
// and copied into a single string at the end. R247-PERF-5.
func procPidPath(pid int, leaf string) string {
	// "/proc/" (6) + "9223372036854775807" (19, max int64) + "/" (1) + leaf
	var buf [6 + 19 + 1 + 16]byte
	b := append(buf[:0], "/proc/"...)
	b = strconv.AppendInt(b, int64(pid), 10)
	b = append(b, '/')
	b = append(b, leaf...)
	return string(b)
}

// ProcStartTime reads the start time (field 22) from /proc/{pid}/stat.
// This value uniquely identifies a process instance even after PID reuse.
// Field 2 (comm) may contain spaces/parentheses, so we locate the last ')' first.
//
// Return values are jiffies since system boot. With the default CLK_TCK=100 Hz
// the value reaches MaxSafeJSONInt (2^53-1) only after ~2.85 million years of
// uptime, so JS front-ends (dashboard.js) can safely consume the field via
// JSON.parse without double-precision truncation. See MaxSafeJSONInt in
// scanner.go; proc_test.go pins the invariant.
//
// R247-PERF-5: discovery scan runs on every dashboard tab open + periodic
// refresh, and walks every PID with a session file, so the path-build and
// stat-field-parse hot paths use byte-level scanning instead of fmt.Sprintf
// and strings.Fields(string(...)) (which would copy the whole stat payload).
func ProcStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(procPidPath(pid, "stat"))
	if err != nil {
		return 0, err
	}
	// Find the end of the comm field (last ')') to avoid parsing issues
	// with process names that contain spaces or parentheses.
	idx := bytes.LastIndexByte(data, ')')
	if idx < 0 || idx+2 >= len(data) {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	// Fields after comm start at index 3 (1-based). We need field 22 (starttime),
	// which is field index 22-3 = 19 in the remaining space-separated fields.
	// Byte-level scan: skip any leading SP, then walk one whitespace-delimited
	// field at a time without allocating string copies.
	rest := data[idx+2:]
	const startTimeIdx = 19 // 0-based index in fields after ')'
	field := 0
	for i := 0; i < len(rest); {
		// Skip whitespace runs. /proc/PID/stat uses single-space separators
		// in practice, but the kernel docs only promise whitespace.
		for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
			i++
		}
		if i >= len(rest) {
			break
		}
		start := i
		for i < len(rest) && rest[i] != ' ' && rest[i] != '\t' && rest[i] != '\n' {
			i++
		}
		if field == startTimeIdx {
			return strconv.ParseUint(string(rest[start:i]), 10, 64)
		}
		field++
	}
	return 0, fmt.Errorf("/proc/%d/stat: too few fields", pid)
}

func procPidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
func procKillSIGKILL(pid int)   { _ = syscall.Kill(pid, syscall.SIGKILL) }

// detectCLIName reads /proc/PID/cmdline to determine which CLI binary is
// running. Iterates registered backend.Profile entries in order, returning
// the first one whose DetectInProc predicate matches. Adding a new backend
// requires no change to this function — the Profile's DetectInProc handles
// the matching. See docs/rfc/multi-backend.md §3.4.
func detectCLIName(pid int) string {
	data, err := os.ReadFile(procPidPath(pid, "cmdline"))
	if err != nil {
		return "cli"
	}
	// cmdline is NUL-separated; first field is the binary path.
	if i := bytes.IndexByte(data, 0); i >= 0 {
		data = data[:i]
	}
	bin := filepath.Base(string(data))
	for _, p := range backend.All() {
		if p.DetectInProc != nil && p.DetectInProc(bin) {
			return p.DisplayName
		}
	}
	return "cli"
}
