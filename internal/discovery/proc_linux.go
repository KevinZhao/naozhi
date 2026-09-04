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

// procPidPath builds "/proc/<pid>/<leaf>" via a stack buffer instead of
// fmt.Sprintf; it runs once per PID on the dashboard scan path.
func procPidPath(pid int, leaf string) string {
	// "/proc/" (6) + "9223372036854775807" (19, max int64) + "/" (1) + leaf
	var buf [6 + 19 + 1 + 16]byte
	b := append(buf[:0], "/proc/"...)
	b = strconv.AppendInt(b, int64(pid), 10)
	b = append(b, '/')
	b = append(b, leaf...)
	return string(b)
}

// ProcStartTime reads the start time (field 22, jiffies since boot) from
// /proc/{pid}/stat; it uniquely identifies a process instance even after PID
// reuse. At CLK_TCK=100 Hz it stays below MaxSafeJSONInt for ~2.85M years, so
// dashboard.js can JSON.parse it without truncation (proc_test.go pins this).
// Byte-level scanning avoids copying the whole stat payload per PID.
func ProcStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(procPidPath(pid, "stat"))
	if err != nil {
		return 0, err
	}
	// Skip past comm (last ')'): process names may contain spaces or parentheses.
	idx := bytes.LastIndexByte(data, ')')
	if idx < 0 || idx+2 >= len(data) {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	// Fields after comm start at field 3 (1-based); starttime is field 22,
	// i.e. index 19 in the remaining whitespace-delimited fields.
	rest := data[idx+2:]
	const startTimeIdx = 19 // 0-based index in fields after ')'
	field := 0
	for i := 0; i < len(rest); {
		// /proc/PID/stat uses single spaces in practice, but the kernel only promises whitespace.
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
// running: the first registered backend.Profile whose DetectInProc matches
// the binary basename wins. See docs/rfc/multi-backend.md §3.4.
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
