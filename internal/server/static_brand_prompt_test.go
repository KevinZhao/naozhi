// static_brand_prompt_test.go — brand prompt-glyph consistency invariant.
//
// The naozhi wordmark is rendered as a terminal prompt: the ">_" glyph
// (GREATER-THAN SIGN + LOW LINE) followed by "naozhi". This motif appears in
// four surfaces — the sidebar header (CSS ::before), the login lockup
// (dashboard.js), the quick-ask empty state (dashboard.html), and the PWA
// metadata (title / manifest). All four must use the SAME ">_" glyph.
//
// A prior revision mistakenly used "›_" (U+203A SINGLE RIGHT-POINTING ANGLE
// QUOTATION MARK) in the sidebar while every other surface used ">". The two
// glyphs are near-identical in many monospace fonts, so the drift went
// unnoticed visually. This invariant test pins the glyph so the brand prompt
// stays a single consistent mark.
//
// This is a glyph-consistency invariant, NOT a DOM-structure source-grep
// (cf. the moratorium in static_ux_contract_test.go) — it locks one brand
// character across surfaces, not implementation layout.
package server

import (
	"embed"
	"strings"
	"testing"
)

// brandWrongAnglePrompt is the look-alike that must never reappear: U+203A
// followed by an underscore. Kept as a rune literal so the test itself does
// not embed the forbidden byte sequence as a copy-paste hazard.
const brandWrongAnglePrompt = "›_"

func readStatic(t *testing.T, fs embed.FS, path string) string {
	t.Helper()
	b, err := fs.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestBrandPrompt_GlyphIsAsciiGreaterThanEverywhere(t *testing.T) {
	t.Parallel()

	assets := map[string]string{
		"dashboard.html": readStatic(t, dashboardHTML, "static/dashboard.html"),
		"dashboard.js":   readStatic(t, dashboardJS, "static/dashboard.js"),
		"manifest.json":  readStatic(t, manifestJSON, "static/manifest.json"),
	}

	for name, src := range assets {
		if strings.Contains(src, brandWrongAnglePrompt) {
			t.Errorf("%s contains the look-alike prompt glyph %q (U+203A); "+
				"the brand prompt must use ASCII '>' so all surfaces match",
				name, brandWrongAnglePrompt)
		}
	}
}

// TestBrandPrompt_TitleAndManifestCarryWordmark pins that the PWA-facing
// surfaces (browser tab title, installed-app name) actually carry the
// ">_naozhi" wordmark rather than a bare "naozhi". The title escapes the
// '>' as the HTML entity "&gt;"; the manifest is JSON and stores it raw.
func TestBrandPrompt_TitleAndManifestCarryWordmark(t *testing.T) {
	t.Parallel()

	html := readStatic(t, dashboardHTML, "static/dashboard.html")
	if !strings.Contains(html, "<title>&gt;_naozhi") {
		t.Error("dashboard.html <title> must carry the &gt;_naozhi wordmark")
	}
	if !strings.Contains(html, `name="application-name" content="&gt;_naozhi"`) {
		t.Error("dashboard.html must declare application-name=&gt;_naozhi")
	}
	if !strings.Contains(html, `name="apple-mobile-web-app-title" content="&gt;_naozhi"`) {
		t.Error("dashboard.html must declare apple-mobile-web-app-title=&gt;_naozhi")
	}

	manifest := readStatic(t, manifestJSON, "static/manifest.json")
	if !strings.Contains(manifest, `">_naozhi`) {
		t.Error("manifest.json name/short_name must carry the >_naozhi wordmark")
	}
}
