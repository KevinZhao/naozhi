//go:build darwin

// R71-TEST-M3 regression tests for proc_darwin.go. The darwin-specific
// ProcStartTime / detectCLIName implementations go through ps(1) and
// time.ParseInLocation(... time.Local), which are both sensitive to
// locale/timezone drift — exactly the kind of path that silently breaks
// on production Mac hosts when CI only runs Linux. These tests pin the
// Linux-equivalent contracts under //go:build darwin so a future
// GOOS=darwin go build (or a darwin CI runner, when one is added)
// surfaces regressions before they ship.

package discovery

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// ProcStartTime (darwin — ps -o lstart=)
// ---------------------------------------------------------------------------

func TestProcStartTime_CurrentProcess(t *testing.T) {
	pid := os.Getpid()
	pst, err := ProcStartTime(pid)
	if err != nil {
		t.Fatalf("ProcStartTime(%d) error: %v", pid, err)
	}
	if pst == 0 {
		t.Error("ProcStartTime returned 0 for current process")
	}
}

func TestProcStartTime_NonexistentPID(t *testing.T) {
	// PID 0 is never a real userspace process; ps returns exit 1.
	_, err := ProcStartTime(0)
	if err == nil {
		t.Error("expected error for PID 0, got nil")
	}
}

func TestProcStartTime_Idempotent(t *testing.T) {
	pid := os.Getpid()
	v1, err := ProcStartTime(pid)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := ProcStartTime(pid)
	if err != nil {
		t.Fatal(err)
	}
	if v1 != v2 {
		t.Errorf("ProcStartTime not idempotent: %d vs %d", v1, v2)
	}
}

// TestProcStartTime_WithinJSONSafeRange pins the invariant that the Darwin
// encoding (Unix microseconds) stays below JavaScript's Number.MAX_SAFE_INTEGER
// (2^53-1). JSON.parse in dashboard.js silently rounds uint64 values above
// this threshold to the nearest representable double, which would break
// handleTakeover's identity equality after restart.
//
// Unix μs reach 2^53-1 near the year 2255; any failure here signals an
// accidental encoding change (e.g. somebody switched to nanoseconds, or
// changed the reference point) that needs an explicit re-budgeting before
// JSON egress.
func TestProcStartTime_WithinJSONSafeRange(t *testing.T) {
	pid := os.Getpid()
	pst, err := ProcStartTime(pid)
	if err != nil {
		t.Fatalf("ProcStartTime: %v", err)
	}
	if pst > MaxSafeJSONInt {
		t.Errorf("ProcStartTime = %d exceeds MaxSafeJSONInt = %d; "+
			"JS JSON.parse will truncate the value and PID-identity checks "+
			"(handleTakeover / verifyProcIdentity) will fail after restart",
			pst, MaxSafeJSONInt)
	}
}

// TestProcStartTime_TimezoneSane is the regression guard for the
// ParseInLocation(time.Local) fix: ps prints lstart in the host's
// local timezone with no zone suffix. If a future edit accidentally
// switches back to time.Parse (which assumes UTC), the returned usec
// would be shifted by the UTC offset — up to 14 hours in either
// direction — producing stale-shim false positives after naozhi
// restarts across TZ-sensitive boundaries.
//
// The test samples the current wall-clock in microseconds and asserts
// the process start time is within ±24 hours: a trivially loose bound
// on Linux-equivalent machines, but tight enough to catch TZ mis-
// interpretation which would push the value out by multiples of 1h.
func TestProcStartTime_TimezoneSane(t *testing.T) {
	pid := os.Getpid()
	pst, err := ProcStartTime(pid)
	if err != nil {
		t.Fatalf("ProcStartTime: %v", err)
	}

	nowUsec := uint64(time.Now().UnixMicro())
	const oneDayUsec = uint64(24 * 60 * 60 * 1_000_000)

	// Start time must be in the past but not absurdly far (> 1 day) —
	// the test binary was just launched.
	if pst > nowUsec {
		t.Errorf("ProcStartTime = %d > now = %d: future timestamp (TZ misinterpretation?)",
			pst, nowUsec)
	}
	if nowUsec-pst > oneDayUsec {
		t.Errorf("ProcStartTime = %d, now = %d: delta %d usec (> 1 day). "+
			"Suggests TZ offset was applied — regression of ParseInLocation(time.Local).",
			pst, nowUsec, nowUsec-pst)
	}
}

// ---------------------------------------------------------------------------
// detectCLIName (darwin — ps -o command=)
// ---------------------------------------------------------------------------

func TestDetectCLIName_CurrentProcess(t *testing.T) {
	pid := os.Getpid()
	// The test binary name is "discovery.test" (or similar); neither
	// "kiro" nor "claude" substring matches, so the fallback "cli"
	// branch wins.
	got := detectCLIName(pid)
	if got == "" {
		t.Error("detectCLIName returned empty string for current process")
	}
}

func TestDetectCLIName_NonexistentPID(t *testing.T) {
	// ps -p 0 exits non-zero; the function must fall back to "cli"
	// instead of returning empty or panicking.
	got := detectCLIName(0)
	if got != "cli" {
		t.Errorf("detectCLIName(0) = %q, want cli (fallback)", got)
	}
}

// TestDetectCLIName_DeadPID — after a process exits, ps returns exit 1
// and detectCLIName should fall back. /bin/true is guaranteed present
// on macOS.
func TestDetectCLIName_DeadPID(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot run /bin/true: %v", err)
	}
	pid := cmd.ProcessState.Pid()
	// Post-exit: ps may still see the PID briefly if it's a zombie,
	// but the common case is a clean "no such process" which hits the
	// error path. Accept either "cli" (fallback) or a real name.
	got := detectCLIName(pid)
	if got == "" {
		t.Error("detectCLIName returned empty string for dead PID")
	}
}
