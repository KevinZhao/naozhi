package agentroute

import "testing"

// TestResolveAgent pins the behaviour contract of the single source of truth
// (R202606b-ARCH-5, #2194). The cases mirror the table that the former
// cron↔session parity test enumerated, but assert concrete expected outputs
// rather than just fork-equivalence.
func TestResolveAgent(t *testing.T) {
	t.Parallel()

	cmds := map[string]string{
		"review":  "code-reviewer",
		"plan":    "planner",
		"general": "general",
	}

	tests := []struct {
		in        string
		wantAgent string
		wantText  string
	}{
		// no prefix
		{"", "general", ""},
		{"hello world", "general", "hello world"},
		{"just some text with /slash not at start", "general", "just some text with /slash not at start"},
		// recognised commands, varied spacing / casing
		{"/review", "code-reviewer", ""},
		{"/review fix the bug", "code-reviewer", "fix the bug"},
		{"/Review fix the bug", "code-reviewer", "fix the bug"}, // CJK IME auto-capitalisation
		{"/REVIEW shout", "code-reviewer", "shout"},             // all-caps
		{"/plan a feature", "planner", "a feature"},             // multi-word rest
		{"/plan", "planner", ""},                                // command, no rest
		{"/general explicit", "general", "explicit"},            // explicit general
		// unrecognised commands fall back to general with the ORIGINAL text
		{"/unknown do thing", "general", "/unknown do thing"},
		{"/notacmd", "general", "/notacmd"},
		// edge punctuation / whitespace forms
		{"/", "general", "/"}, // bare slash (cmd "" not in map)
		{"/ leading space rest", "general", "/ leading space rest"}, // slash + space, empty cmd
		{"/review  double space", "code-reviewer", " double space"}, // double space: only first split consumed
		{"/review\ttab-rest", "general", "/review\ttab-rest"},       // tab is not the space delimiter → token "review\ttab-rest"
		{"/révïew unicode cmd", "general", "/révïew unicode cmd"},   // non-ASCII command token, unrecognised
		{"/review 中文正文", "code-reviewer", "中文正文"},                   // CJK rest
		{"  /review leading ws", "general", "  /review leading ws"}, // leading whitespace before slash → no prefix
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			gotAgent, gotText := ResolveAgent(tc.in, cmds)
			if gotAgent != tc.wantAgent || gotText != tc.wantText {
				t.Errorf("ResolveAgent(%q)\n  got  = (%q, %q)\n  want = (%q, %q)",
					tc.in, gotAgent, gotText, tc.wantAgent, tc.wantText)
			}
		})
	}
}
