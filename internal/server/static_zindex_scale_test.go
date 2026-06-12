// static_zindex_scale_test.go — overlay z-index scale guard (R20260610-UI-2).
//
// Overlays previously each picked an ad-hoc z-index; the lightbox shipped a
// wild 9999 that sat orders of magnitude above every other layer, so stacking
// order across overlays was unpredictable and impossible to reason about. The
// UI-2 fix introduced an ordered --nz-z-* scale (popover < drawer < toast <
// menu < overlay < lightbox) and routed every full-screen / blocking overlay
// through it.
//
// This guard pins two invariants so the scale can't silently erode:
//  1. No rule re-introduces the wild 9999 literal (or any z-index ≥ 1000).
//  2. The six scale tokens are defined in :root in strictly ascending order,
//     so the documented layering (lightbox on top) holds.
package server

import (
	"regexp"
	"strconv"
	"testing"
)

var (
	reZIndexLiteral = regexp.MustCompile(`z-index:(\d+)`)
	// Matches `--nz-z-NAME:VALUE;` token definitions in :root.
	reZIndexToken = regexp.MustCompile(`--nz-z-([a-z]+):(\d+)`)
)

func TestDashboardHTML_ZIndexScale(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	// (1) No wild z-index literal. Anything ≥ 1000 is the kind of "9999" escape
	// hatch the scale exists to prevent — route it through --nz-z-lightbox (the
	// topmost tier) instead.
	for _, m := range reZIndexLiteral.FindAllStringSubmatch(html, -1) {
		v, _ := strconv.Atoi(m[1])
		if v >= 1000 {
			t.Errorf("wild z-index literal %d found — use the --nz-z-* scale (e.g. var(--nz-z-lightbox)) instead of a magic number", v)
		}
	}

	// (2) The scale tokens exist and ascend in the documented order. A token
	// defined out of order would invert layering (e.g. a popover painting over
	// the lightbox).
	want := []string{"popover", "drawer", "toast", "menu", "overlay", "lightbox"}
	got := map[string]int{}
	for _, m := range reZIndexToken.FindAllStringSubmatch(html, -1) {
		v, _ := strconv.Atoi(m[2])
		got[m[1]] = v
	}
	prev := -1
	for _, name := range want {
		v, ok := got[name]
		if !ok {
			t.Errorf("z-index scale token --nz-z-%s not defined in :root", name)
			continue
		}
		if v <= prev {
			t.Errorf("z-index scale token --nz-z-%s=%d does not exceed the previous tier (%d) — scale must ascend %v", name, v, prev, want)
		}
		prev = v
	}
}
