package cron

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

// TestResolveAllowedRoot_EvalSymlinksFailure_LogsAndFallsBack pins
// R112714-LOGIC-4: when filepath.EvalSymlinks fails for cfg.AllowedRoot
// (e.g. path does not exist), the previous code silently returned "" which
// disabled the allowed-root sandbox for the entire scheduler lifetime with
// no operator-visible signal.
//
// The fix must:
//  1. Emit a slog.Warn (not silently return "").
//  2. Return cfg.AllowedRoot (the raw string) rather than "" so the
//     constraint is still enforced via bare string comparison — weaker than
//     symlink-resolved comparison but strictly better than no constraint.
//
// Not t.Parallel: this test swaps the process-global slog default to capture
// log output; running it in parallel races with other parallel tests whose
// NewScheduler/slog calls write to the shared default logger's buffer.
func TestResolveAllowedRoot_EvalSymlinksFailure_LogsAndFallsBack(t *testing.T) {
	nonExistentPath := "/tmp/naozhi-test-nonexistent-allowedroot-" + mustGenerateID()

	// Confirm the path really does not exist so the test is deterministic.
	if _, err := os.Stat(nonExistentPath); !os.IsNotExist(err) {
		t.Skipf("test precondition: path %q unexpectedly exists; skipping", nonExistentPath)
	}

	// Capture slog output to verify a warning is emitted.
	var logBuf strings.Builder
	handler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	origDefault := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(origDefault) })

	cfg := &SchedulerConfig{AllowedRoot: nonExistentPath}
	got := cfg.resolveAllowedRoot()

	// 1. Must return the raw path, not "".
	if got != nonExistentPath {
		t.Errorf("resolveAllowedRoot(%q) = %q; want raw path %q (fallback on EvalSymlinks failure)",
			nonExistentPath, got, nonExistentPath)
	}

	// 2. Must have logged a warning that mentions the path and the error.
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "EvalSymlinks") {
		t.Errorf("resolveAllowedRoot: expected slog output to mention EvalSymlinks; got: %q", logOutput)
	}
	if !strings.Contains(logOutput, nonExistentPath) {
		t.Errorf("resolveAllowedRoot: expected slog output to contain the configured path %q; got: %q",
			nonExistentPath, logOutput)
	}
}

// TestResolveAllowedRoot_ExistingPath_ReturnsResolved verifies the happy path:
// when EvalSymlinks succeeds the resolved (real) path is returned, not the raw input.
func TestResolveAllowedRoot_ExistingPath_ReturnsResolved(t *testing.T) {
	t.Parallel()

	// /tmp is guaranteed to exist on all platforms used by this project.
	cfg := &SchedulerConfig{AllowedRoot: "/tmp"}
	got := cfg.resolveAllowedRoot()
	if got == "" {
		t.Error("resolveAllowedRoot(\"/tmp\") returned \"\"; expected a non-empty resolved path")
	}
}

// TestNewScheduler_AllowedRootNonExistent_UsesRawFallback confirms that the
// NewScheduler constructor propagates the raw-path fallback through to
// s.allowedRoot when EvalSymlinks fails, so the runtime sandbox constraint
// is still active (even if weaker than symlink-resolved).
// Not t.Parallel: swaps the process-global slog default (to suppress the
// resolveAllowedRoot warning), which races with other parallel tests' slog
// calls under -race.
func TestNewScheduler_AllowedRootNonExistent_UsesRawFallback(t *testing.T) {
	nonExistentPath := "/tmp/naozhi-test-nonexistent-nr-" + mustGenerateID()
	if _, err := os.Stat(nonExistentPath); !os.IsNotExist(err) {
		t.Skipf("test precondition: path %q unexpectedly exists; skipping", nonExistentPath)
	}

	// Suppress the slog.Warn from resolveAllowedRoot so test output is clean.
	origDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(origDefault) })

	s := NewScheduler(SchedulerConfig{
		AllowedRoot:    nonExistentPath,
		MaxJobs:        5,
		AllowNilRouter: true,
	}, SchedulerDeps{})
	// Pre-fix: s.allowedRoot would be "" (sandbox disabled).
	// Post-fix: s.allowedRoot should equal the raw path.
	if s.allowedRoot == "" {
		t.Errorf("R112714-LOGIC-4: s.allowedRoot = %q after NewScheduler with "+
			"non-existent AllowedRoot; expected the raw path %q so the "+
			"sandbox constraint is still enforced (raw string comparison) "+
			"rather than silently disabled.",
			s.allowedRoot, nonExistentPath)
	}
	if s.allowedRoot != nonExistentPath {
		t.Errorf("s.allowedRoot = %q; want %q", s.allowedRoot, nonExistentPath)
	}
}
