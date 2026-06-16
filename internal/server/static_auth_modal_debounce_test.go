// static_auth_modal_debounce_test.go — auth-prompt de-dupe + debounce guard.
//
// Operators reported the token modal popping open repeatedly: the nz_auth
// cookie expired on a fixed 1h timer (fixed server-side via sliding renewal)
// and the 5s /api/sessions poll re-fired showAuthModal on every 401. This
// guard pins the front-end half of the fix:
//
//  1. showAuthModal de-dupes (one overlay at a time) and honours a cooldown
//     for background (auto) re-prompts.
//  2. The background poll calls showAuthModal({ auto: true }) so a dismissed
//     prompt isn't reopened by the next poll tick.
//  3. Dismissing the modal arms the cooldown; a successful login clears it.
package server

import (
	"strings"
	"testing"
)

func TestDashboardJS_AuthModalDebounce(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1. The cooldown state + de-dupe guard must exist inside showAuthModal.
	i := strings.Index(js, "function showAuthModal(")
	if i < 0 {
		t.Fatal("showAuthModal not found — auth modal renamed/removed")
	}
	body := js[i:]
	if end := strings.Index(body, "\nfunction "); end >= 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "querySelector('.modal-overlay')") {
		t.Error("showAuthModal lost its single-overlay de-dupe guard — prompts can stack")
	}
	if !strings.Contains(body, "_authModalCooldownUntil") {
		t.Error("showAuthModal lost the background-reprompt cooldown — 401 polls can machine-gun the modal open")
	}

	// 2. The background /api/sessions poll must opt into the cooldown via
	//    { auto: true } rather than prompting unconditionally.
	if !strings.Contains(js, "showAuthModal({ auto: true })") {
		t.Error("background poll no longer calls showAuthModal({ auto: true }) — dismissed prompt will be reopened by the next poll")
	}

	// 3. Dismiss arms the cooldown; successful login clears it.
	if !strings.Contains(js, "function dismissAuthModal(") {
		t.Error("dismissAuthModal helper missing — cancel button won't arm the reprompt cooldown")
	}
	if !strings.Contains(js, "_authModalCooldownUntil = 0") {
		t.Error("successful login no longer clears _authModalCooldownUntil — re-login could be suppressed by a stale cooldown")
	}
}
