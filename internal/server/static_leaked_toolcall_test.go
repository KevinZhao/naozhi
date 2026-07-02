package server

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/leakguard"
)

// Leaked-tool-call rendering guard.
//
// Background: the claude/anthropic harness expresses a real tool call as a
// structured content block (type:"tool_use"), which naozhi surfaces as its
// own tool_use event and filters out of the main transcript. The model
// occasionally regresses and writes the call syntax —
//
//	call
//	<invoke name="Bash">
//	<parameter name="command">…</parameter>
//	</invoke>
//
// — verbatim into an assistant *text* block. That text is stored as
// type:"text" (process_event_format.go) and previously rendered as a literal
// wall of XML in the bubble. dashboard.js's stripLeakedToolCalls detects this
// and folds the malformed payload behind a collapsed <details>.
//
// These tests pin the two halves of the contract that actually matter:
//  1. The JS function + CSS fold classes exist (string contract — catches a
//     rename / accidental deletion that would silently revert the fold).
//  2. The detection *boundary* is correct (Go re-implementation of the same
//     regex, table-driven over real-world samples): genuine leaks are caught,
//     and quoted tool-call syntax inside prose (e.g. a bug report discussing
//     `<invoke …>` in backticks) is NEVER flagged. A false positive here is
//     worse than the bug — it would shred legitimate technical discussion.

// detectLeak delegates to the runtime detector in internal/leakguard — the
// single source of truth shared by the backend auto-continue path and this
// drift guard. TestLeakguardAnchor_MatchesJS below asserts leakguard.Anchor
// still equals the LEAKED_TOOLCALL_RE literal in dashboard.js, so JS ⇄ Go ⇄
// runtime cannot drift.
func detectLeak(text string) bool {
	return leakguard.Detect(text)
}

func TestDashboardJS_LeakedToolCall_DetectionBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		leak bool
	}{
		// --- genuine leaks: must be caught ---
		{
			name: "call marker then invoke with parameters",
			text: "先读取它的完整范围。\n\ncall\n<invoke name=\"Read\">\n<parameter name=\"file_path\">/tmp/x.go</parameter>\n</invoke>",
			leak: true,
		},
		{
			name: "self-closing invoke no parameters (ExitPlanMode shape)",
			text: "让我先退出计划模式。\n\ncall\n<invoke name=\"ExitPlanMode\">\n</invoke>",
			leak: true,
		},
		{
			name: "function_calls wrapper marker",
			text: "running now.\n<function_calls>\n<invoke name=\"Bash\">\n<parameter name=\"command\">ls</parameter>\n</invoke>\n</function_calls>",
			leak: true,
		},
		{
			name: "leak at very start of message",
			text: "call\n<invoke name=\"Bash\">\n<parameter name=\"command\">echo hi</parameter>\n</invoke>",
			leak: true,
		},

		// --- must NOT be flagged: quoted syntax in legitimate prose ---
		{
			name: "backtick-quoted invoke in a bug report",
			text: "截图里显示的是 `<invoke name=\"Bash\">` 这种 XML 字面量,夹在文本气泡里。",
			leak: false,
		},
		{
			name: "invoke discussed inline without call-line anchor and no close tag",
			text: "那段字面 `\\ncall\\n<invoke name=\"Read\">\\n<parameter ...` 整个嵌在 text 字段里。",
			leak: false,
		},
		{
			name: "the word call in prose followed later by quoted invoke",
			text: "I will call the tool. The syntax is `<invoke name=\"X\">` which closes with `</invoke>`.",
			leak: false,
		},
		{
			name: "plain prose no tool syntax",
			text: "这是一条完全正常的回复,讨论了 transcript 渲染与边界问题。",
			leak: false,
		},
		{
			name: "empty",
			text: "",
			leak: false,
		},
		{
			name: "close tag present but no call-line anchor (bare invoke quoted)",
			text: "compare `<invoke name=\"a\">x</invoke>` against the structured form.",
			leak: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := detectLeak(tc.text); got != tc.leak {
				t.Errorf("detectLeak(%q) = %v, want %v", tc.text, got, tc.leak)
			}
		})
	}
}

// TestDashboardJS_LeakedToolCall_FunctionAndStylesPresent pins that the fold
// machinery is wired: the detector function exists, the text branch calls it,
// and the CSS fold classes are defined. A rename that breaks any leg silently
// reverts the bubble to dumping raw XML.
func TestDashboardJS_LeakedToolCall_FunctionAndStylesPresent(t *testing.T) {
	t.Parallel()
	js, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	s := string(js)
	if !strings.Contains(s, "function stripLeakedToolCalls(") {
		t.Error("stripLeakedToolCalls function missing from dashboard.js — leaked tool-call folding would be lost")
	}
	// The text/user branch must actually invoke the detector, otherwise the
	// function exists but nothing folds.
	if !strings.Contains(s, "stripLeakedToolCalls(cleanRaw)") {
		t.Error("eventHtml text branch must call stripLeakedToolCalls(cleanRaw) — fold is dead code otherwise")
	}
	// The fold emits these classes; the CSS in dashboard.html must define them.
	if !strings.Contains(s, "leaked-toolcall-summary") || !strings.Contains(s, "leaked-toolcall-body") {
		t.Error("eventHtml must emit leaked-toolcall-summary / leaked-toolcall-body fold markup")
	}

	html, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	hs := string(html)
	for _, cls := range []string{".leaked-toolcall{", ".leaked-toolcall-summary", ".leaked-toolcall-body"} {
		if !strings.Contains(hs, cls) {
			t.Errorf("dashboard.html missing CSS for %q — fold would render unstyled", cls)
		}
	}
}

// TestDashboardJS_LeakedToolCall_RegexInSync asserts the JS source still
// carries the exact anchor this Go test re-implements, so the Go detectLeak
// copy cannot drift from the shipped behaviour without a test failure.
func TestDashboardJS_LeakedToolCall_RegexInSync(t *testing.T) {
	t.Parallel()
	js, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	// The canonical anchor is now owned by internal/leakguard (the runtime
	// detector). Assert the JS literal still contains it verbatim; if a future
	// edit relaxes/tightens the JS pattern, update leakguard.Anchor (and the
	// leakguard test samples) in the same change.
	if !strings.Contains(string(js), leakguard.Anchor) {
		t.Errorf("LEAKED_TOOLCALL_RE in dashboard.js drifted from leakguard.Anchor.\nExpected JS to contain anchor:\n  %s\nUpdate leakguard.Anchor + samples to match.", leakguard.Anchor)
	}
}

// TestLeakguardAnchor_MatchesJS is the JS→Go leg of the three-way lock: the
// runtime detector's Anchor must appear verbatim in the shipped dashboard.js,
// so the backend auto-continue path and the frontend cosmetic fold can never
// diverge on what counts as a leak.
func TestLeakguardAnchor_MatchesJS(t *testing.T) {
	t.Parallel()
	js, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	if !strings.Contains(string(js), leakguard.Anchor) {
		t.Errorf("leakguard.Anchor not found verbatim in dashboard.js:\n  %s", leakguard.Anchor)
	}
}
