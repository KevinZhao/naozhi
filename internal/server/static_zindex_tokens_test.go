package server

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// zIndexRe matches a z-index declaration with a literal value.
var zIndexRe = regexp.MustCompile(`z-index:\s*(\d+)`)

// localStackingMax is the ceiling for a literal z-index: values at or below it
// order siblings INSIDE one stacking context (a sticky drawer header over its
// scrolled rows, a badge over its card), where a global token would be
// misleading — the number only has meaning next to its neighbours. Anything
// above it competes with other views for the global order and must come from
// the --nz-z-* scale in css/tokens.css, so the whole order is readable in one
// place (#2559 D6-2).
const localStackingMax = 11

// TestDashboardCSS_GlobalZIndexUsesTokens keeps cross-view layering in the
// token scale. Before #2559 the stylesheets carried 27 literals including six
// global ones (30 / 50 / 99 / 199 / 202 / 203); those became named tokens with
// their values unchanged, and this test stops new ones from appearing.
func TestDashboardCSS_GlobalZIndexUsesTokens(t *testing.T) {
	t.Parallel()
	for _, name := range dashboardCSSFiles {
		data := staticAssetBytes(name)
		if data == nil {
			t.Fatalf("%s not embedded", name)
		}
		css := string(data)
		for _, m := range zIndexRe.FindAllStringSubmatchIndex(css, -1) {
			raw := css[m[2]:m[3]]
			v, err := strconv.Atoi(raw)
			if err != nil {
				continue
			}
			if v > localStackingMax {
				line := 1 + strings.Count(css[:m[0]], "\n")
				t.Errorf("%s:%d: z-index:%d is a global layer — use a var(--nz-z-*) token from css/tokens.css "+
					"(literals are only for ordering siblings inside one stacking context, <= %d)",
					name, line, v, localStackingMax)
			}
		}
	}
}

// TestDashboardCSS_ZIndexTokensDefinedAndOrdered pins the scale itself: every
// --nz-z-* token a stylesheet references must be defined, and the tiers must
// stay in the documented low→high order. A token that silently reorders (say
// modal below drawer) is exactly the class of regression the scale exists to
// prevent.
func TestDashboardCSS_ZIndexTokensDefinedAndOrdered(t *testing.T) {
	t.Parallel()
	tokens := staticAssetBytes("css/tokens.css")
	if tokens == nil {
		t.Fatal("css/tokens.css not embedded")
	}
	defined := map[string]int{}
	for _, m := range regexp.MustCompile(`--nz-z-([\w-]+):\s*(\d+)`).FindAllStringSubmatch(string(tokens), -1) {
		v, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("--nz-z-%s has a non-numeric value %q", m[1], m[2])
		}
		defined[m[1]] = v
	}
	if len(defined) == 0 {
		t.Fatal("css/tokens.css defines no --nz-z-* tokens")
	}

	// Every referenced token exists.
	for _, name := range dashboardCSSFiles {
		css := string(staticAssetBytes(name))
		for _, m := range regexp.MustCompile(`var\(--nz-z-([\w-]+)\)`).FindAllStringSubmatch(css, -1) {
			if _, ok := defined[m[1]]; !ok {
				t.Errorf("%s references undefined token --nz-z-%s", name, m[1])
			}
		}
	}

	// Documented ordering, low → high. Only tiers that must not swap are listed;
	// the local-ordering tokens (rail / chip-pop) sit below and are checked as a
	// group against popover.
	order := []string{"rail", "chip-pop", "backdrop", "popover", "backdrop-sheet", "drawer", "split-front", "split-resizer", "modal", "menu", "overlay", "lightbox", "toast"}
	prev := -1
	prevName := ""
	for _, name := range order {
		v, ok := defined[name]
		if !ok {
			t.Errorf("expected tier --nz-z-%s to be defined", name)
			continue
		}
		if v <= prev {
			t.Errorf("--nz-z-%s (%d) must sit above --nz-z-%s (%d) — the scale's documented order changed", name, v, prevName, prev)
		}
		prev, prevName = v, name
	}
}
