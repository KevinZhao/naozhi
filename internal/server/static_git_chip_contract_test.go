// static_git_chip_contract_test.go — wiring pins for the header git chip.
//
// dashboard.js has no JS unit runner, so these Go source-greps guard the two
// invariants that a silent refactor could break without any test failing:
//  1. the chip is fetched and repainted at every point the header is rebuilt
//     (a missing repaint blanks the chip on rename, which reads as "not a
//     repo" — a wrong answer, not a missing one)
//  2. every dynamic value reaching the chip HTML passes through esc/escAttr
//     (branch names and directory names are filesystem-controlled)
//
// Per the #388 guidance in static_ux_contract_test.go this file stays small
// and asserts wiring, not DOM shape.
package server

import (
	"regexp"
	"strings"
	"testing"
)

func TestDashboardJS_GitChipWiring(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	for _, want := range []string{
		// Endpoint + the async fill path.
		`'/api/sessions/git?key='`,
		`function fetchGitState(`,
		`function gitChipHtml(`,
		`function setHeaderGitChip(`,
		// Header mount node built empty by renderMainShell.
		`id="header-git"`,
		`getElementById('header-git')`,
		// Fetched on session select and repainted after every header rebuild.
		`fetchGitState(key, node)`,
		`repaintGitChip()`,
		// Workspace moves (/cd) invalidate the cached state.
		`function invalidateGitState(`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard.js missing git-chip wiring: %q", want)
		}
	}

	// The mount node must exist exactly once. A second `id="header-git"` (e.g.
	// copied into previewDiscovered's hand-rolled header) would make
	// getElementById pick whichever came first in the DOM and let a discovered
	// session inherit the managed session's branch chip.
	if n := strings.Count(js, `id="header-git"`); n != 1 {
		t.Errorf(`id="header-git" appears %d times, want exactly 1 (duplicate mount = wrong session's chip)`, n)
	}
}

// TestDashboardJS_GitChipEscapesDynamicValues pins that no branch / worktree /
// path value is interpolated into the chip HTML raw. These strings come from
// the filesystem (a refname or directory name an operator can create), so a
// missing esc() would be a stored-XSS vector in the dashboard header.
func TestDashboardJS_GitChipEscapesDynamicValues(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	body, ok := funcBody(js, "function gitChipHtml(")
	if !ok {
		t.Fatal("gitChipHtml not found in dashboard.js")
	}

	// Every `g.<field>` reference that lands in returned HTML must be wrapped.
	// The tooltip is assembled in tipParts and escaped once via escAttr at the
	// end, so we assert on the two sinks rather than each field.
	for _, want := range []string{
		`escAttr(tipParts.join(`,
		`esc(g.worktree)`,
		`esc(branch)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("gitChipHtml missing escaping call %q — filesystem-controlled value would be interpolated raw", want)
		}
	}

	// Negative: no raw `g.<field>` may appear in the returned markup. Scope to
	// the `return '<span…` expression — the tooltip assembly above it builds
	// plain strings in tipParts and is escaped once by escAttr, so matching the
	// whole body would flag those legitimately-unescaped reads.
	at := strings.Index(body, "return '<span")
	if at < 0 {
		t.Fatal("gitChipHtml has no `return '<span` markup expression — update this contract")
	}
	ret := body[at:]
	rawInterp := regexp.MustCompile(`\bg\.\w+`)
	if m := rawInterp.FindString(ret); m != "" {
		t.Errorf("gitChipHtml interpolates a raw payload field into HTML (%q) — wrap it in esc()/escAttr()", m)
	}
}

// funcBody returns the source between the given function header and its
// closing brace at column 0 (dashboard.js uses top-level function
// declarations, so `\n}` terminates the body unambiguously).
func funcBody(js, header string) (string, bool) {
	i := strings.Index(js, header)
	if i < 0 {
		return "", false
	}
	rest := js[i:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}
