package cron

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestResolveAgent_ParityWithSession pins R250531-ARCH-02 (#1506).
//
// cron.resolveAgent is a deliberate verbatim fork of session.ResolveAgent so
// that the cron package keeps zero production import edge onto internal/session
// (see agent_resolve.go godoc). The risk that motivated the fork is that the
// two copies silently drift: a semantic change in session.ResolveAgent (new
// prefix form, unicode-space trimming, different case-folding, ...) would make
// a `/agent` prefix route one way over the IM/dispatch path and another way
// when the same text is stored as a cron job — with zero CI signal.
//
// This test closes that gap WITHOUT re-introducing a production import: a
// _test.go importing internal/session does not appear in the cron package's
// production dependency graph (other cron test files already import session),
// so the fork stays intact while the parity is enforced. If either copy
// changes semantics, this test fails and forces both copies to be reconciled.
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
