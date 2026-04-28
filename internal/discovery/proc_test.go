//go:build linux

package discovery

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// ProcStartTime (Linux only - reads /proc/PID/stat)
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
	// PID 0 is never a real userspace process.
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

// TestProcStartTime_WithinJSONSafeRange pins the invariant that the Linux
// encoding (jiffies since boot) stays below JavaScript's Number.MAX_SAFE_INTEGER
// (2^53-1). JSON.parse in dashboard.js silently rounds uint64 values above
// this threshold to the nearest representable double, which would break
// handleTakeover's identity equality after restart.
//
// With CLK_TCK=100 Hz the budget would only be consumed after ~2.85 million
// years of system uptime — any failure here signals an accidental encoding
// change (e.g. somebody switched field 22 to nanoseconds, or changed the
// reference point) that needs an explicit re-budgeting before JSON egress.
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

// ---------------------------------------------------------------------------
// detectCLIName (Linux only - reads /proc/PID/cmdline)
// ---------------------------------------------------------------------------

func TestDetectCLIName_CurrentProcess(t *testing.T) {
	pid := os.Getpid()
	// The test binary is "discovery.test" or similar — not "claude" or "kiro",
	// so it should return the "cli" fallback.
	got := detectCLIName(pid)
	// We don't know the exact value, just that it returns a non-empty string.
	if got == "" {
		t.Error("detectCLIName returned empty string for current process")
	}
}

func TestDetectCLIName_NonexistentPID(t *testing.T) {
	got := detectCLIName(0)
	if got != "cli" {
		t.Errorf("detectCLIName(0) = %q, want cli (fallback)", got)
	}
}

// ---------------------------------------------------------------------------
// processAlive
// ---------------------------------------------------------------------------

func TestProcessAlive_CurrentProcess(t *testing.T) {
	pid := os.Getpid()
	if !processAlive(pid) {
		t.Error("processAlive returned false for the current process")
	}
}

func TestProcessAlive_DeadPID(t *testing.T) {
	// Start a short-lived process and wait for it to exit.
	cmd := exec.Command("/bin/true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot run /bin/true: %v", err)
	}
	pid := cmd.ProcessState.Pid()
	// After cmd.Run() the process has exited; PID may be reused but that's OK
	// for this test — we're checking the function doesn't panic.
	_ = processAlive(pid)
}

// ---------------------------------------------------------------------------
// sortByLastActive
// ---------------------------------------------------------------------------

func TestSortByLastActive(t *testing.T) {
	candidates := []scanCandidate{
		{lastActive: 300},
		{lastActive: 100},
		{lastActive: 200},
	}
	indices := []int{0, 1, 2}
	sortByLastActive(indices, candidates)
	// Most stale first = smallest lastActive first
	if candidates[indices[0]].lastActive != 100 {
		t.Errorf("index[0] lastActive = %d, want 100", candidates[indices[0]].lastActive)
	}
	if candidates[indices[1]].lastActive != 200 {
		t.Errorf("index[1] lastActive = %d, want 200", candidates[indices[1]].lastActive)
	}
	if candidates[indices[2]].lastActive != 300 {
		t.Errorf("index[2] lastActive = %d, want 300", candidates[indices[2]].lastActive)
	}
}

// ---------------------------------------------------------------------------
// WaitAndCleanup
// ---------------------------------------------------------------------------

func TestWaitAndCleanup_AlreadyDeadProcess(t *testing.T) {
	// Run a quick process and let it exit.
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start /bin/true: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	claudeDir := makeClaudeDir(t)
	// Write a session file to exercise the os.Remove path.
	sessDir := filepath.Join(claudeDir, "sessions")
	sessFile := filepath.Join(sessDir, fmt.Sprintf("%d.json", pid))
	if err := os.WriteFile(sessFile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	cwd := "/tmp/cleanup-test"
	sessionID := "12345678-0000-0000-0000-000000000001"

	ctx := context.Background()
	// Should complete quickly because the process is already dead.
	done := make(chan struct{})
	go func() {
		WaitAndCleanup(ctx, pid, 0, claudeDir, cwd, sessionID)
		close(done)
	}()

	select {
	case <-done:
		// Good — completed without hanging.
	case <-time.After(10 * time.Second):
		t.Fatal("WaitAndCleanup did not complete within timeout")
	}

	// Session file should have been removed.
	if _, err := os.Stat(sessFile); !os.IsNotExist(err) {
		t.Errorf("session file should have been removed, err = %v", err)
	}
}

func TestWaitAndCleanup_ContextCancelled(t *testing.T) {
	// Start a long-running process and cancel the context immediately.
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start /bin/sleep: %v", err)
	}
	pid := cmd.Process.Pid
	defer cmd.Process.Kill() //nolint:errcheck

	claudeDir := makeClaudeDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	done := make(chan struct{})
	go func() {
		WaitAndCleanup(ctx, pid, 0, claudeDir, "/tmp/ctx-cancel", "00000000-0000-0000-0000-000000000099")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("WaitAndCleanup should complete quickly on cancelled context")
	}
}

// ---------------------------------------------------------------------------
// waitForExit
// ---------------------------------------------------------------------------

func TestWaitForExit_AlreadyDead(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start /bin/true: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait() // wait for it to die

	ctx := context.Background()
	cancelled := waitForExit(ctx, pid)
	// Process is dead so waitForExit should return false.
	if cancelled {
		t.Error("expected waitForExit to return false (process dead, not ctx cancelled)")
	}
}

func TestWaitForExit_ContextCancelled(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start /bin/sleep: %v", err)
	}
	pid := cmd.Process.Pid
	defer cmd.Process.Kill() //nolint:errcheck

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediate cancellation

	start := time.Now()
	cancelled := waitForExit(ctx, pid)
	elapsed := time.Since(start)

	if !cancelled {
		t.Error("expected waitForExit to return true (ctx cancelled)")
	}
	if elapsed > 2*time.Second {
		t.Errorf("waitForExit took too long after ctx cancel: %v", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Scan (smoke test with empty sessions dir)
// ---------------------------------------------------------------------------

func TestScan_EmptySessionsDir(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	// sessions dir exists but is empty
	sessions, err := Scan(claudeDir, nil, nil, nil)
	if err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions from empty dir, got %d", len(sessions))
	}
}

func TestScan_NonexistentSessionsDir(t *testing.T) {
	dir := t.TempDir()
	// No sessions/ subdirectory at all
	sessions, err := Scan(dir, nil, nil, nil)
	if err != nil {
		t.Fatalf("Scan error for missing sessions dir: %v", err)
	}
	if sessions != nil {
		t.Errorf("expected nil, got %v", sessions)
	}
}

func TestScan_SkipsExcludedPID(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	sessDir := filepath.Join(claudeDir, "sessions")

	// Write a session file for the current process (definitely alive)
	pid := os.Getpid()
	sf := sessionFile{
		PID:        pid,
		SessionID:  "aaaaaaaa-1234-1234-1234-000000000001",
		CWD:        "/tmp/scan-skip",
		StartedAt:  time.Now().UnixMilli(),
		Kind:       "interactive",
		Entrypoint: "cli",
	}
	makeSessionFile(t, sessDir, sf)

	// Exclude this PID — session should not appear
	sessions, err := Scan(claudeDir, map[int]bool{pid: true}, nil, nil)
	if err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	for _, s := range sessions {
		if s.PID == pid && s.SessionID == sf.SessionID {
			t.Errorf("excluded PID %d appeared in scan results", pid)
		}
	}
}

func TestScan_SkipsNonCLIEntrypoint(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	sessDir := filepath.Join(claudeDir, "sessions")

	pid := os.Getpid()
	sf := sessionFile{
		PID:        pid,
		SessionID:  "aaaaaaaa-1234-1234-1234-000000000002",
		CWD:        "/tmp/scan-skip-entrypoint",
		StartedAt:  time.Now().UnixMilli(),
		Kind:       "interactive",
		Entrypoint: "sdk-ts", // should be filtered out
	}
	makeSessionFile(t, sessDir, sf)

	sessions, err := Scan(claudeDir, nil, nil, nil)
	if err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	for _, s := range sessions {
		if s.SessionID == sf.SessionID {
			t.Errorf("sdk-ts session should have been skipped, but appeared: %+v", s)
		}
	}
}

func TestScan_IncludesCLISession(t *testing.T) {
	resetCaches(t)
	claudeDir := makeClaudeDir(t)
	sessDir := filepath.Join(claudeDir, "sessions")

	// Use current process so processAlive returns true
	pid := os.Getpid()
	cwd := "/tmp/scan-include"
	sessionID := "aaaaaaaa-1234-1234-1234-000000000003"
	sf := sessionFile{
		PID:        pid,
		SessionID:  sessionID,
		CWD:        cwd,
		StartedAt:  time.Now().UnixMilli(),
		Kind:       "interactive",
		Entrypoint: "cli",
	}
	makeSessionFile(t, sessDir, sf)

	// Create matching project dir and JSONL file
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(projDir, sessionID+".jsonl")
	makeJSONLWithUserPrompts(t, jsonlPath, []string{"scan test prompt"})

	sessions, err := Scan(claudeDir, nil, nil, nil)
	if err != nil {
		t.Fatalf("Scan error: %v", err)
	}

	found := false
	for _, s := range sessions {
		if s.SessionID == sessionID && s.PID == pid {
			found = true
			if s.LastPrompt != "scan test prompt" {
				t.Errorf("LastPrompt = %q, want scan test prompt", s.LastPrompt)
			}
			if s.ProcStartTime == 0 {
				t.Error("ProcStartTime should be non-zero")
			}
		}
	}
	if !found {
		t.Logf("all sessions found: %+v", sessions)
		t.Errorf("expected session %q with PID %d in scan results", sessionID, pid)
	}
}

func TestScan_ProcStartTimeInStat(t *testing.T) {
	// Verify that /proc/self/stat field parsing works correctly.
	// This is a Linux-only test that double-checks our field indexing.
	pidStr := strconv.Itoa(os.Getpid())
	data, err := os.ReadFile("/proc/" + pidStr + "/stat")
	if err != nil {
		t.Skipf("cannot read /proc/self/stat: %v", err)
	}
	// The file should contain at least 22 space-separated fields after the comm.
	if len(data) < 10 {
		t.Fatalf("/proc/self/stat too short: %q", data)
	}
	pst, err := ProcStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("ProcStartTime: %v", err)
	}
	if pst == 0 {
		t.Error("ProcStartTime returned 0")
	}
}
