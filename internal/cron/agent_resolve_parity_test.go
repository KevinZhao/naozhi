package cron

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestResolveAgent_ParityWithSession pins R250531-ARCH-02 (#1506),
// R202606b-ARCH-5 (#2194).
//
// History: cron.resolveAgent used to be a verbatim FORK of
// session.ResolveAgent, kept in sync only by the enumerated table below — a
// drift hazard (a new prefix form added to one copy and not the other would
// route a `/agent` prefix one way over IM/dispatch and another when the same
// text is stored as a cron job, with zero CI signal). #2194 removed the fork:
// both functions now delegate to the single source of truth in
// internal/agentroute (a leaf package importing only "strings"), so divergence
// is structurally impossible while the cron package still carries zero
// production import edge onto internal/session.
//
// This test is retained as a belt-and-suspenders regression net: it still
// imports session only as a _test dependency (absent from cron's production
// dependency graph), and now asserts the two delegating wrappers stay
// observably equivalent — catching any future re-fork or a wrapper that
// accidentally stops delegating.
func TestResolveAgent_ParityWithSession(t *testing.T) {
	t.Parallel()

	cmds := map[string]string{
		"review":  "code-reviewer",
		"plan":    "planner",
		"general": "general",
	}

	cases := []string{
		// no prefix
		"",
		"hello world",
		"just some text with /slash not at start",
		// recognised commands, varied spacing / casing
		"/review",
		"/review fix the bug",
		"/Review fix the bug", // CJK IME auto-capitalisation
		"/REVIEW shout",       // all-caps
		"/plan a feature",     // multi-word rest
		"/plan",               // command, no rest
		"/general explicit",   // explicit general
		// unrecognised commands
		"/unknown do thing",
		"/notacmd",
		// edge punctuation / whitespace forms
		"/",                     // bare slash
		"/ leading space rest",  // slash + space
		"/review  double space", // double space rest
		"/review\ttab-rest",     // tab after command token
		"/révïew unicode cmd",   // non-ASCII command token
		"/review 中文正文",          // CJK rest
		"  /review leading ws",  // leading whitespace before slash (no prefix)
	}

	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			cronAgent, cronText := resolveAgent(in, cmds)
			sessAgent, sessText := session.ResolveAgent(in, cmds)
			if cronAgent != sessAgent || cronText != sessText {
				t.Fatalf("fork drift for %q:\n  cron    = (%q, %q)\n  session = (%q, %q)\n"+
					"cron.resolveAgent and session.ResolveAgent must stay byte-for-byte "+
					"equivalent (see agent_resolve.go #1506)",
					in, cronAgent, cronText, sessAgent, sessText)
			}
		})
	}
}
