package discovery

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oversizedUserLine builds a single JSONL "user" record whose serialized
// length exceeds maxJSONLLineBytes by padding an inline base64-style image
// block. Mirrors what the Claude CLI writes when a user uploads images:
// the base64 bytes are inlined into the same NDJSON line, which can balloon
// a single line past 5-10 MB.
func oversizedUserLine(padBytes int) string {
	blob := strings.Repeat("A", padBytes)
	return fmt.Sprintf(
		`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":%q}},{"type":"text","text":"look at this"}]}}`,
		blob,
	)
}

// TestParseJSONL_OversizedLineDoesNotPoisonFile is the regression test for
// the dashboard "preview load history" failure: a JSONL whose Nth line is a
// multi-MB inline-image record must not abort parsing of every other line.
//
// Before the fix, parseJSONL used a bufio.Scanner with a 4 MB cap; a single
// oversized line made Scan() return false with "bufio.Scanner: token too
// long", parseJSONL returned that error, and the handler rendered a blank
// "暂无会话历史" splash for the entire session.
func TestParseJSONL_OversizedLineDoesNotPoisonFile(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	lines := []string{
		userJSONLLine("user", "first message before the big one"),
		oversizedUserLine(6 * 1024 * 1024), // 6 MB single line > 4 MB cap
		assistantJSONLLine("reply after the big one"),
		userJSONLLine("user", "third message after the big one"),
	}
	_, jsonlPath := makeSessionJSONL(t, claudeDir, "-home-user-proj", "11111111-1111-1111-1111-111111111111", lines)

	entries, err := parseJSONL(jsonlPath)
	if err != nil {
		t.Fatalf("parseJSONL returned error on oversized line (regression): %v", err)
	}

	// Exactly the three normal records survive (no phantom/duplicate entry
	// from a partially-drained oversized line), in chronological order; the
	// oversized line is skipped (its text block is collateral — matching
	// parseTail's drain-and-skip behaviour for the same 4 MB threshold).
	var summaries []string
	for _, e := range entries {
		summaries = append(summaries, e.Summary)
	}
	want := []string{
		"first message before the big one",
		"reply after the big one",
		"third message after the big one",
	}
	if len(summaries) != len(want) {
		t.Fatalf("expected exactly %d entries, got %d: %q", len(want), len(summaries), summaries)
	}
	for i, w := range want {
		if summaries[i] != w {
			t.Errorf("entry %d: want %q, got %q (order/content drift)", i, w, summaries[i])
		}
	}
	joined := strings.Join(summaries, " | ")
	if strings.Contains(joined, "look at this") {
		t.Errorf("oversized line's text should be skipped, but it leaked into entries: %q", joined)
	}
}

// TestParseJSONL_OversizedLineAtPositions verifies the skip works regardless
// of where the oversized line sits — head, middle, tail (no trailing
// newline) — and that the surrounding records always load.
func TestParseJSONL_OversizedLineAtPositions(t *testing.T) {
	big := oversizedUserLine(5 * 1024 * 1024)
	normalA := userJSONLLine("user", "alpha")
	normalB := assistantJSONLLine("bravo")

	cases := []struct {
		name  string
		lines []string
		// whether the file ends without a trailing newline
		noTrailingNewline bool
	}{
		{name: "head", lines: []string{big, normalA, normalB}},
		{name: "middle", lines: []string{normalA, big, normalB}},
		{name: "tail_with_newline", lines: []string{normalA, normalB, big}},
		{name: "tail_no_newline", lines: []string{normalA, normalB, big}, noTrailingNewline: true},
		{name: "two_oversized", lines: []string{big, normalA, big, normalB, big}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claudeDir := makeClaudeDir(t)
			projDir := filepath.Join(claudeDir, "projects", "-p")
			if err := os.MkdirAll(projDir, 0o755); err != nil {
				t.Fatal(err)
			}
			jsonlPath := filepath.Join(projDir, "22222222-2222-2222-2222-222222222222.jsonl")
			content := strings.Join(tc.lines, "\n")
			if !tc.noTrailingNewline {
				content += "\n"
			}
			if err := os.WriteFile(jsonlPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			entries, err := parseJSONL(jsonlPath)
			if err != nil {
				t.Fatalf("parseJSONL error: %v", err)
			}
			var joined string
			for _, e := range entries {
				joined += e.Summary + " | "
			}
			if !strings.Contains(joined, "alpha") || !strings.Contains(joined, "bravo") {
				t.Errorf("normal records lost around oversized line(s): %q", joined)
			}
		})
	}
}

// TestExtractFirstPrompt_OversizedFirstLine ensures the sidebar first-prompt
// extraction also tolerates a leading oversized image line: it must skip the
// big line and find the first real text prompt that follows, rather than
// returning empty.
func TestExtractFirstPrompt_OversizedFirstLine(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	lines := []string{
		oversizedUserLine(5 * 1024 * 1024),
		userJSONLLine("user", "the real first prompt"),
	}
	_, jsonlPath := makeSessionJSONL(t, claudeDir, "-home-user-proj2", "33333333-3333-3333-3333-333333333333", lines)

	got := extractFirstPrompt(jsonlPath)
	if !strings.Contains(got, "the real first prompt") {
		t.Errorf("extractFirstPrompt should skip oversized line and find the next prompt; got %q", got)
	}
}

// TestParseJSONL_AllOversizedReturnsEmptyNotError locks the core contract of
// the bug fix: a file that is ENTIRELY oversized lines (a pure image-dump
// session) yields empty history with NO error, so the dashboard renders the
// "暂无会话历史" splash rather than blanking on a load error. The final line
// has no trailing newline to exercise the EOF-mid-oversized-line path.
func TestParseJSONL_AllOversizedReturnsEmptyNotError(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	projDir := filepath.Join(claudeDir, "projects", "-imgdump")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(projDir, "44444444-4444-4444-4444-444444444444.jsonl")
	content := oversizedUserLine(5*1024*1024) + "\n" +
		oversizedUserLine(5*1024*1024) + "\n" +
		oversizedUserLine(5*1024*1024) // no trailing newline

	if err := os.WriteFile(jsonlPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := parseJSONL(jsonlPath)
	if err != nil {
		t.Fatalf("all-oversized file must not error (got %v)", err)
	}
	if len(entries) != 0 {
		t.Fatalf("all-oversized file must yield 0 entries, got %d", len(entries))
	}
}

// TestReadJSONLLine_Boundary pins the exact threshold semantics that the
// "agree with parseTail" guarantee depends on: a line whose total length is
// exactly maxJSONLLineBytes is KEPT (oversized=false), and maxJSONLLineBytes+1
// is DROPPED (oversized=true). A future flip of `>` to `>=`, or a change to
// the constant, must fail here.
func TestReadJSONLLine_Boundary(t *testing.T) {
	cases := []struct {
		name        string
		lineLen     int
		wantSkipped bool
	}{
		{name: "one_under_cap", lineLen: maxJSONLLineBytes - 1, wantSkipped: false},
		{name: "exactly_cap", lineLen: maxJSONLLineBytes, wantSkipped: false},
		{name: "one_over_cap", lineLen: maxJSONLLineBytes + 1, wantSkipped: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Build a payload of exactly tc.lineLen bytes followed by '\n'.
			payload := bytes.Repeat([]byte("x"), tc.lineLen)
			r := bufio.NewReaderSize(bytes.NewReader(append(payload, '\n')), 64*1024)
			line, oversized, err := readJSONLLine(r)
			if oversized != tc.wantSkipped {
				t.Fatalf("lineLen=%d: oversized=%v want %v (err=%v)", tc.lineLen, oversized, tc.wantSkipped, err)
			}
			if !tc.wantSkipped && len(line) != tc.lineLen {
				t.Fatalf("kept line should be %d bytes, got %d", tc.lineLen, len(line))
			}
			if tc.wantSkipped && len(line) != 0 {
				t.Fatalf("skipped line should return nil bytes, got %d", len(line))
			}
		})
	}
}

// TestExtractFirstPrompt_BudgetExhausted documents the firstPromptScanBudget
// guard: when the head is filled with sub-cap non-user turns whose bytes
// exceed the budget before any user prompt appears, extractFirstPrompt bails
// with "" instead of scanning the whole file.
func TestExtractFirstPrompt_BudgetExhausted(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	// Each assistant line ~200 KB of (sub-cap) text; a handful blow past the
	// 512 KB budget. The user prompt sits AFTER them, so a budget-respecting
	// scan must give up and return "".
	bigText := strings.Repeat("z", 200*1024)
	var lines []string
	for i := 0; i < 4; i++ { // ~800 KB > 512 KB budget
		lines = append(lines, assistantJSONLLine(bigText))
	}
	lines = append(lines, userJSONLLine("user", "prompt beyond the budget"))
	_, jsonlPath := makeSessionJSONL(t, claudeDir, "-budget", "55555555-5555-5555-5555-555555555555", lines)

	if got := extractFirstPrompt(jsonlPath); got != "" {
		t.Errorf("expected empty prompt once scan budget is exhausted, got %q", got)
	}
}
