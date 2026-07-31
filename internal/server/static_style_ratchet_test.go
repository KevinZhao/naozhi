// static_style_ratchet_test.go — design-system literal ratchet (R20260606).
//
// The R20260606 design-system pass introduced --nz-fs-* / --nz-radius-* /
// --nz-space-* tokens and migrated every exact-value font-size / border-radius
// literal to them. A residue of off-scale literals remains (e.g. 13px, 6px,
// 3px) that need a render-changing decision before they can fold into the
// scale, so we can't forbid literals outright yet.
//
// Instead this is a ratchet: the count of raw `font-size:Npx` /
// `border-radius:Npx` literals may only go DOWN. Adding a fresh literal
// instead of using a token trips the test; folding more literals into tokens
// lowers the baseline (update the const below in the same commit). This keeps
// the scale from eroding while allowing incremental cleanup.
package server

import (
	"regexp"
	"testing"
)

// Baselines captured immediately after the R20260606 migration. LOWER these
// (never raise) as remaining off-scale literals fold into tokens.
const (
	maxFontSizeLiterals     = 30
	maxBorderRadiusLiterals = 24
	// ui-polish-light-theme D2: floating-surface shadows moved to the
	// --nz-shadow-* tiers (with light-theme overrides). The one survivor is
	// the lightbox — image viewing stays a dark theatre in both themes.
	// A new floating surface must reference a tier, not a fresh black literal.
	maxBlackShadowLiterals = 1
)

var (
	reFontSizeLiteral     = regexp.MustCompile(`font-size:\d+px`)
	reBorderRadiusLiteral = regexp.MustCompile(`border-radius:\d+px`)
	reBlackShadowLiteral  = regexp.MustCompile(`box-shadow:[^;}]*rgba\(0,0,0`)
)

func TestDashboardHTML_StyleLiteralRatchet(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	if got := len(reFontSizeLiteral.FindAllString(html, -1)); got > maxFontSizeLiterals {
		t.Errorf("raw font-size:Npx literals = %d, exceeds ratchet %d — use a --nz-fs-* token instead of a px literal (see :root design-system scale)", got, maxFontSizeLiterals)
	} else if got < maxFontSizeLiterals {
		t.Logf("font-size literals dropped to %d (< baseline %d) — lower maxFontSizeLiterals to %d to lock the gain", got, maxFontSizeLiterals, got)
	}

	if got := len(reBorderRadiusLiteral.FindAllString(html, -1)); got > maxBorderRadiusLiterals {
		t.Errorf("raw border-radius:Npx literals = %d, exceeds ratchet %d — use a --nz-radius-* token instead of a px literal", got, maxBorderRadiusLiterals)
	} else if got < maxBorderRadiusLiterals {
		t.Logf("border-radius literals dropped to %d (< baseline %d) — lower maxBorderRadiusLiterals to %d to lock the gain", got, maxBorderRadiusLiterals, got)
	}

	if got := len(reBlackShadowLiteral.FindAllString(html, -1)); got > maxBlackShadowLiterals {
		t.Errorf("black box-shadow literals = %d, exceeds ratchet %d — use a --nz-shadow-* tier (light theme overrides them) instead of rgba(0,0,0,…)", got, maxBlackShadowLiterals)
	} else if got < maxBlackShadowLiterals {
		t.Logf("black box-shadow literals dropped to %d (< baseline %d) — lower maxBlackShadowLiterals to %d to lock the gain", got, maxBlackShadowLiterals, got)
	}
}
