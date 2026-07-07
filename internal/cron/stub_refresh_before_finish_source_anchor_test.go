package cron

import (
	"os"
	"regexp"
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
// call must be followed (in source order) by a finishRun(finishArgs{...}) call
// before the next stubRefresh.run() appears. A regression that moves any
// stub-refresh call back below its finishRun fails here without needing an
// end-to-end race.
func TestErrorPaths_StubRefreshBeforeFinishRun_SourceAnchor(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("scheduler_run.go")
	if err != nil {
		t.Fatalf("read scheduler_run.go: %v", err)
	}
	// Strip Go line comments (// ...) before matching. scheduler_run.go has
	// comment prose that mentions `stubRefresh.run()` as documentation (e.g.
	// the GO-009b note around line 1008 and the preflight rationale near line
	// 162); those would inflate stubMatches and let the >=5 guard pass even if
	// a real call site were deleted or misplaced (R202606i-GO-002 / #2333).
	// We only want to anchor against actual code, so blank out the comment tail
	// of each line while preserving offsets (replace with spaces) so the
	// before/finish ordering arithmetic below stays valid.
	body := stripLineComments(string(src))

	// Collect the source offsets of every error/cancel-path stub refresh call
	// (the value receiver `stubRefresh.run()` in execSendError, the struct-field
	// `a.stubRefresh.run()` in executeGetSession, and the local `refresh.run()`
	// the preflight helper now fires on its delete-mid-execute failure branch —
	// R202606h-GO-009b / #2318) and every finishRun(finishArgs{...}) call.
	reStub := regexp.MustCompile(`(?:a\.stubRefresh|stubRefresh|\brefresh)\.run\(\)`)
	stubMatches := reStub.FindAllStringIndex(body, -1)
	if len(stubMatches) == 0 {
		t.Fatal("scheduler_run.go: no stubRefresh.run() call found — refactor removed the error-path stub re-register?")
	}
	finishMatches := regexp.MustCompile(`s\.finishRun\(finishArgs\{`).FindAllStringIndex(body, -1)
	if len(finishMatches) == 0 {
		t.Fatal("scheduler_run.go: no finishRun(finishArgs{...}) call found")
	}

	// FIVE error/cancel paths each re-register the stub once and then finishRun:
	//   1. execSendError cancel branch          (stubRefresh.run())
	//   2. execSendError non-cancel branch       (stubRefresh.run())
	//   3. executeGetSession cancel branch       (a.stubRefresh.run())
	//   4. executeGetSession non-cancel branch   (a.stubRefresh.run())
	//   5. freshContextPreflightP0 delete branch (refresh.run() — #2318)
	//
	// The structural invariant: for every stub-refresh call there must be a
	// finishRun(finishArgs{...}) between it and the NEXT stub call (or EOF) —
	// i.e. the stub re-register precedes the finishRun that releases the CAS
	// gate. Pre-fix, each of these had finishRun BEFORE the stub (the region
	// after the stub up to the next stub had no finishRun), so the count was
	// lower. We now require >=5 to also pin the preflight-helper fix (#2318).
	stubBeforeFinish := 0
	for i, sm := range stubMatches {
		regionEnd := len(body)
		if i+1 < len(stubMatches) {
			regionEnd = stubMatches[i+1][0]
		}
		for _, fm := range finishMatches {
			if fm[0] > sm[0] && fm[0] < regionEnd {
				stubBeforeFinish++
				break
			}
		}
	}
	if stubBeforeFinish < 5 {
		t.Errorf("scheduler_run.go: only %d stub-refresh calls precede their branch finishRun; expected >=5 (execSendError + executeGetSession cancel/non-cancel paths plus the freshContextPreflightP0 delete-mid-execute branch). A stub refresh placed AFTER finishRun releases the CAS gate reopens the phantom-stub race (R202606h-GO-009/GO-009b/GO-010).", stubBeforeFinish)
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
