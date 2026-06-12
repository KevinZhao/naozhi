// static_overlay_behavior_test.go — overlay behavior parity guard (R20260610-UI-3).
//
// The dashboard's blocking overlays had inconsistent behavior: the scrim was
// dimmed at three different opacities (modal/voice .75, cmd-palette .6) for the
// same "a blocking layer is open" cue, and the voice-overlay — alone among the
// overlays — had no Esc-to-close handler, so a stuck recording could only be
// dismissed by clicking it. The UI-3 fix unified the blocking-modal scrim onto
// a single --nz-scrim token and wired Esc into the voice overlay.
//
// This guard pins both fixes:
//  1. modal / voice / cmd-palette overlays route their scrim through
//     var(--nz-scrim) instead of a bare rgba literal.
//  2. The global Esc keydown handler references the voice-overlay so a stuck
//     recording is dismissable by keyboard.
package server

import (
	"strings"
	"testing"
)

func TestDashboardHTML_BlockingScrimToken(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	if !strings.Contains(html, "--nz-scrim:") {
		t.Fatal("--nz-scrim token not defined in :root — UI-3 scrim unification missing")
	}

	// Each blocking-modal overlay rule must pull its background from the token.
	for _, sel := range []string{".voice-overlay{", ".modal-overlay{", ".cmd-palette-overlay{"} {
		i := strings.Index(html, sel)
		if i < 0 {
			t.Errorf("selector %q not found — overlay rule renamed/removed", sel)
			continue
		}
		body := html[i:]
		if end := strings.Index(body, "}"); end >= 0 {
			body = body[:end]
		}
		if !strings.Contains(body, "background:var(--nz-scrim)") {
			t.Errorf("%s scrim does not use var(--nz-scrim) — found rule body %q", sel, body)
		}
	}
}

func TestDashboardJS_VoiceOverlayEscClose(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Locate the global Esc keydown handler (keyed off the comment that opens it)
	// and assert the voice-overlay branch lives inside it.
	const marker = "Global Esc:"
	i := strings.Index(js, marker)
	if i < 0 {
		t.Fatal("global Esc handler comment not found — keydown handler renamed")
	}
	// Scan a generous window after the marker for the voice-overlay dismissal.
	window := js[i:]
	if len(window) > 1500 {
		window = window[:1500]
	}
	if !strings.Contains(window, "voice-overlay") {
		t.Error("global Esc handler does not reference voice-overlay — Esc-to-close for the recording overlay is missing (UI-3)")
	}
}
