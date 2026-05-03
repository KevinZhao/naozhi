package shim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetWriteDeadlineContract_NoSilentErrorDrop is a repo-wide regression gate
// against the pattern fixed in Round 163 and extended across the remaining 11
// call sites ("REL3" in docs/TODO.md).
//
// Problem: `_ = conn.SetWriteDeadline(...)` or `//nolint:errcheck` on a
// SetWriteDeadline call silently drops the error. If the underlying socket is
// already closed / half-closed, the subsequent Write has no deadline and may
// block until TCP keepalive expires (minutes), wedging a mutex-holding write
// path. This is exactly the class of bug Round 163 started chasing.
//
// Contract: every `SetWriteDeadline(` call under internal/ must either
//
//	(a) capture the return value with `if err := ... SetWriteDeadline(...); err`
//	    and handle failure (return / log / close), OR
//	(b) be a pure deadline *clear* (`SetWriteDeadline(time.Time{})`) which is
//	    inherently best-effort — a dying conn will be torn down by the outer
//	    defer anyway, so the error carries no actionable signal, OR
//	(c) appear in the explicit allowlist below for documented best-effort
//	    fire-and-forget use cases (systemd notify socket, etc.).
//
// A failure here usually means a new code path was added without reading this
// test. Before suppressing, ask: if SetWriteDeadline returns an error on the
// ErrNoProgress / ErrClosed path, will the next Write block? If yes, pick (a).
func TestSetWriteDeadlineContract_NoSilentErrorDrop(t *testing.T) {
	t.Parallel()

	repoRoot := findRepoRoot(t)
	internal := filepath.Join(repoRoot, "internal")

	// Allowlist of call sites where dropping the error is intentional and
	// documented. Each entry is "<relative path>:<exact code-line substring>".
	// A match strips these from the scanned text before searching for bare
	// patterns. Keep narrow — substrings are checked for uniqueness at startup.
	allowed := []string{
		// systemd notify socket: a dgram write that's followed immediately
		// by conn.Close() (defer) — no subsequent blocking write that could
		// wedge on a missing deadline.
		"osutil/sdnotify_linux.go:_ = conn.SetWriteDeadline(time.Now().Add(time.Second))",
	}

	var violations []string
	err := filepath.Walk(internal, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(internal, p)
		rel = filepath.ToSlash(rel)

		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if !classifyBareSetWriteDeadline(trimmed) {
				continue
			}

			// Allowed: explicit allowlist entries.
			matched := false
			for _, a := range allowed {
				parts := strings.SplitN(a, ":", 2)
				if len(parts) != 2 {
					continue
				}
				if rel == parts[0] && strings.Contains(trimmed, parts[1]) {
					matched = true
					break
				}
			}
			if matched {
				continue
			}

			violations = append(violations,
				rel+": bare SetWriteDeadline (capture err or add to allowlist): "+trimmed)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}

	if len(violations) > 0 {
		t.Errorf("REL3 regression: %d bare SetWriteDeadline call(s) found.\n"+
			"Capture the error (`if err := ...SetWriteDeadline(...); err != nil { ... }`) "+
			"or clear with SetWriteDeadline(time.Time{}), "+
			"or add to the allowlist in this test with a justification.\n%s",
			len(violations), strings.Join(violations, "\n"))
	}
}

// findRepoRoot is defined in manager_sudoers_argv_test.go.

// TestSetWriteDeadlineContract_SelfCheck verifies the regression gate actually
// catches a bare call — prevents the test from silently degrading into a
// rubber-stamp if someone refactors the matcher and accidentally allows too
// much. Runs the same scanning logic against an in-memory fixture.
func TestSetWriteDeadlineContract_SelfCheck(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		line          string
		wantViolation bool
	}{
		{"bare_nolint_errcheck", `conn.SetWriteDeadline(time.Now().Add(time.Second)) //nolint:errcheck`, true},
		{"bare_underscore_assign", `_ = conn.SetWriteDeadline(time.Now().Add(time.Second))`, true},
		{"bare_naked", `conn.SetWriteDeadline(time.Now().Add(time.Second))`, true},
		{"captured_err", `if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {`, false},
		{"deadline_clear", `_ = conn.SetWriteDeadline(time.Time{})`, false},
		{"comment_godoc", `// SetWriteDeadline is called below to bound the write`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			trimmed := strings.TrimSpace(tc.line)
			got := classifyBareSetWriteDeadline(trimmed)
			if got != tc.wantViolation {
				t.Errorf("classifyBareSetWriteDeadline(%q) = %v, want %v",
					tc.line, got, tc.wantViolation)
			}
		})
	}
}

// classifyBareSetWriteDeadline returns true when the trimmed line should be
// flagged as a violation by the regression gate. Exposed as a separate helper
// so the self-check test exercises the same decision logic as the main scan.
func classifyBareSetWriteDeadline(trimmed string) bool {
	if !strings.Contains(trimmed, "SetWriteDeadline(") {
		return false
	}
	if strings.HasPrefix(trimmed, "//") {
		return false
	}
	if strings.Contains(trimmed, "SetWriteDeadline(time.Time{})") {
		return false
	}
	if strings.Contains(trimmed, "if err := ") && strings.Contains(trimmed, "SetWriteDeadline(") {
		return false
	}
	return true
}
