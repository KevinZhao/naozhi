// anchor-keep: pins that the stub re-register happens BEFORE finishRun releases the CAS gate — a statement ordering whose violation is a race with TriggerNow, not a deterministic behaviour.
package cron

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestErrorPaths_StubRefreshBeforeFinishRun_SourceAnchor is the
// R202606h-GO-009/GO-010 source anchor: on the fresh-context error and cancel
// paths, the sidebar stub re-registration (stubRefresh.run()) MUST run BEFORE
// the finishRun that releases the inflight CAS gate (finishRun →
// finalizer.finalize() → running.Store(false)).
//
// Why: a late stubRefresh.run() AFTER the gate is freed opens a window where a
// concurrent TriggerNow wins the CAS, runs its own preflight Reset +
// GetOrCreate (spawning run-B's live session and registering its live sidebar
// stub), and then run-A's stale-chain stubRefresh.run() blindly overwrites that
// live stub with snap-time lastSessionID — a phantom sidebar row pointing at
// the PRIOR session's JSONL. This mirrors the success-path contract
// (R050103A-COUPLING-1 / #1911), where reapFreshSessionLocked (Reset + stub
// re-register) precedes finishRun; the error paths must follow the same rule.
//
// A true concurrency race for the finalize()→stubRefresh.run() window is hard
// to reproduce deterministically without injecting a hook in the gap, so this
// pins the contract structurally: every stubRefresh.run() / a.stubRefresh.run()
// call must be followed (in source order) by a finishRun(...) call
// before the next stubRefresh.run() appears. A regression that moves any
// stub-refresh call back below its finishRun fails here without needing an
// end-to-end race.
func TestErrorPaths_StubRefreshBeforeFinishRun_SourceAnchor(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("scheduler_run.go")
	if err != nil {
		t.Fatalf("read scheduler_run.go: %v", err)
	}
	// Comments are blanked (offsets preserved) so prose mentioning refresh.run()
	// cannot satisfy the check (R202606i-GO-002 / #2333).
	body := stripLineComments(string(src))

	// Scope to freshContextPreflightP0's body. Its cousin in internal/node had an
	// unbounded `.*?` and matched past its own function into the next one, so it
	// silently guarded nothing (Epic I #2547) — the window is cut explicitly here,
	// from the func header to the next top-level func.
	start := strings.Index(body, "func (s *Scheduler) freshContextPreflightP0(")
	if start < 0 {
		t.Fatal("freshContextPreflightP0 not found in scheduler_run.go — retarget this test")
	}
	rest := body[start:]
	if end := strings.Index(rest[1:], "\nfunc "); end >= 0 {
		rest = rest[:end+1]
	}

	// The delete-mid-execute branch: refresh.run() must precede the terminal call
	// that releases the CAS gate. A stub refreshed afterwards can clobber a
	// concurrent TriggerNow's live stub (R202606h-GO-009b / #2318).
	//
	// This is the ONE path of the original five that behaviour cannot cover:
	// stubRefresher.run() re-registers only if the job still exists, and this
	// branch is reached precisely because it does not, so the call is a designed
	// no-op there and nothing observable happens. The other four are covered by
	// TestFreshContextResetsOnCancel / TestFreshContextResetsOnSendError /
	// TestFreshGetSession_CancelError_… / TestFreshGetSession_SessionError_…,
	// each verified by breaking it.
	//
	// Checked by POSITION, not by counting call sites. An earlier version required
	// ">= 1 stub refresh precedes its finish" across the whole file, which a single
	// misplacement could not fail: the other three sites still satisfied the count.
	refreshIdx := strings.Index(rest, "refresh.run()")
	if refreshIdx < 0 {
		t.Fatal("freshContextPreflightP0: no refresh.run() found — the delete-mid-execute stub re-register was removed (#2318)")
	}
	// Every terminal branch in scheduler_run.go ends in s.finishRun(rc, out).
	reFinish := regexp.MustCompile(`s\.finishRun\(`)
	if reFinish.FindStringIndex(rest[refreshIdx:]) == nil {
		t.Errorf("freshContextPreflightP0: refresh.run() at offset %d is not followed by a terminal finishRun call. "+
			"The stub re-register MUST happen before the CAS gate is released, or a concurrent TriggerNow's live stub gets clobbered (#2318).",
			refreshIdx)
	}
}

// stripLineComments blanks out the `// ...` tail of each line (replacing the
// comment text with spaces so byte offsets are preserved) while leaving any
// `//` that appears inside a double-quoted or back-quoted string literal
// intact. This keeps the source-anchor regex from matching `stubRefresh.run()`
// when it occurs in documentation comments rather than real call sites.
func stripLineComments(s string) string {
	out := []byte(s)
	inStr := false   // inside a "..." literal
	inRaw := false   // inside a `...` literal
	inRune := false  // inside a '...' literal
	escaped := false // previous byte was a backslash inside a "..." literal
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case inRaw:
			if c == '`' {
				inRaw = false
			}
		case inStr:
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inStr = false
			}
		case inRune:
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '\'' {
				inRune = false
			}
		default:
			switch c {
			case '"':
				inStr = true
			case '`':
				inRaw = true
			case '\'':
				inRune = true
			case '/':
				if i+1 < len(out) && out[i+1] == '/' {
					// Blank from here to end of line, preserving the newline.
					for j := i; j < len(out) && out[j] != '\n'; j++ {
						out[j] = ' '
					}
				}
			}
		}
	}
	return string(out)
}

// TestStripLineComments_DropsCommentMatchesKeepsCode pins the #2333 fix:
// stubRefresh.run() in a // comment must be blanked out, while real code
// calls (including inside string literals, which must NOT be treated as
// comments) survive. Offsets are preserved (same length out as in).
func TestStripLineComments_DropsCommentMatchesKeepsCode(t *testing.T) {
	t.Parallel()
	in := "stubRefresh.run() // see stubRefresh.run() note\n" +
		"x := \"a // b\"\n" + // // inside a string literal is not a comment
		"y := `raw // not a comment`\n"
	out := stripLineComments(in)
	if len(out) != len(in) {
		t.Fatalf("length changed: in=%d out=%d (offsets must be preserved)", len(in), len(out))
	}
	re := regexp.MustCompile(`stubRefresh\.run\(\)`)
	if got := len(re.FindAllString(in, -1)); got != 2 {
		t.Fatalf("precondition: raw input should have 2 matches, got %d", got)
	}
	if got := len(re.FindAllString(out, -1)); got != 1 {
		t.Errorf("after stripping comments, stubRefresh.run() matches = %d; want 1 (the comment occurrence must be removed)", got)
	}
	// String/raw-literal content must remain (the `//` inside them isn't a comment).
	if !regexp.MustCompile(`a // b`).MatchString(out) {
		t.Error(`"a // b" string literal content was wrongly stripped`)
	}
	if !regexp.MustCompile(`raw // not a comment`).MatchString(out) {
		t.Error("`raw // ...` backtick literal content was wrongly stripped")
	}
}
