package discovery

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ProcStartTime reads the start time (field 22) from /proc/{pid}/stat.
// This value uniquely identifies a process instance even after PID reuse.
// Field 2 (comm) may contain spaces/parentheses, so we locate the last ')' first.
//
// Return values are jiffies since system boot. With the default CLK_TCK=100 Hz
// the value reaches MaxSafeJSONInt (2^53-1) only after ~2.85 million years of
// uptime, so JS front-ends (dashboard.js) can safely consume the field via
// JSON.parse without double-precision truncation. See MaxSafeJSONInt in
// scanner.go; proc_test.go pins the invariant.
func ProcStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
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
	fields := strings.Fields(string(data[idx+2:]))
	const startTimeIdx = 19 // 0-based index in fields after ')'
	if len(fields) <= startTimeIdx {
		return 0, fmt.Errorf("/proc/%d/stat: too few fields", pid)
	}
	return strconv.ParseUint(fields[startTimeIdx], 10, 64)
}

// detectCLIName reads /proc/PID/cmdline to determine which CLI binary is running.
// Returns "claude-code", "kiro", or "cli" as fallback.
func detectCLIName(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "cli"
	}
	// cmdline is NUL-separated; first field is the binary path.
	if i := bytes.IndexByte(data, 0); i >= 0 {
		data = data[:i]
	}
	bin := filepath.Base(string(data))
	switch {
	case strings.Contains(bin, "kiro"):
		return "kiro"
	case strings.Contains(bin, "claude"):
		return "claude-code"
	default:
		return "cli"
	}
}
