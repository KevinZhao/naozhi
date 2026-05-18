package project

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateConfig_PlannerPromptRejectsLF locks R222-SEC-6 contract:
// ValidateConfig MUST reject PlannerPrompt that contains a literal LF (\n)
// or CR (\r), regardless of whatever relaxation the cron-prompt validator
// (validateCronPrompt in internal/server/dashboard_cron.go) introduces.
//
// Why this is contract-grade and not just one row in TestValidateConfig:
//
//   - PlannerPrompt flows into the CLI argv via `--append-system-prompt`.
//     The shell exec layer treats "\n" as an argv terminator on some
//     parser paths, and our shim writes argv into a single-line proto
//     frame.  If a future maintainer copy-pastes the LF-allowed branch
//     from validateCronPrompt (which is safe because cron prompts go
//     through stdin / stream-json, not argv), the argv path silently
//     breaks framing or — worse — splits one prompt into two arguments.
//
//   - The cron-side comment that mentions "--append-system-prompt single-
//     line constraint" is correct only insofar as PlannerPrompt has this
//     test guarding it.  Removing the LF rejection here without first
//     re-architecting the argv plumbing must fail this test, forcing the
//     reviewer to confront the constraint instead of relying on a comment
//     in the cron handler.
//
// Both LF and CR are tested: \n is the obvious framing splitter; \r is
// rejected because journalctl / `tail -f` interpret it as a carriage
// return that overwrites the current log line, surfacing as a log-injection
// surface even when argv parsing is unaffected.
func TestValidateConfig_PlannerPromptRejectsLF(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		prompt string
	}{
		{"bare LF", "foo\nbar"},
		{"bare CR", "foo\rbar"},
		{"CRLF", "foo\r\nbar"},
		{"trailing LF", "instructions:\n"},
		{"leading LF", "\nbe terse"},
		{"LF with surrounding whitespace", "  \n  "},
		{"LF after long prefix", strings.Repeat("a", 4000) + "\n"},
		{"NEL (U+0085) — should also fail via IsLogInjectionRune slow scan",
			"foobar"},
		{"LS (U+2028) — should fail via IsLogInjectionRune",
			"foo bar"},
		{"PS (U+2029) — should fail via IsLogInjectionRune",
			"foo bar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateConfig(ProjectConfig{PlannerPrompt: c.prompt})
			if err == nil {
				t.Fatalf("ValidateConfig accepted PlannerPrompt %q; "+
					"argv-splitting / log-injection guard regressed", c.prompt)
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("err = %v; want wrapper of ErrInvalidConfig", err)
			}
		})
	}
}

// TestValidateConfig_PlannerPromptAllowsTab is the dual: tab MUST stay
// accepted so playbook prompts with code-style indentation still load.
// If a maintainer tightens the loop above to "no whitespace control runes
// at all", this test fails first and forces them to keep the tab carve-out.
func TestValidateConfig_PlannerPromptAllowsTab(t *testing.T) {
	t.Parallel()
	if err := ValidateConfig(ProjectConfig{
		PlannerPrompt: "step one:\tindent example",
	}); err != nil {
		t.Errorf("ValidateConfig rejected legitimate tab-indented prompt: %v", err)
	}
}
