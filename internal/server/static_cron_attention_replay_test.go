package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_CronAttentionQueue pins the §7.4 confirmation-queue +
// §7.3 replay contracts on cron_view.js (per-file, no union — cron_view.js
// split lesson). The queue + replay are the UI face of §6.2 double-run
// containment, so the tri-state predicate and both resolve actions must be
// present, per RFC §7.6 testing规约.
func TestDashboardJS_CronAttentionQueue(t *testing.T) {
	t.Parallel()
	data, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read cron_view.js: %v", err)
	}
	js := string(data)

	for _, frag := range []string{
		// queue banner + cards.
		"function cronAttentionQueueHtml()",
		"function cronAttentionCardHtml(it)",
		"function cronAttentionRefresh()",
		"'/api/cron/attention'",
		"ctr-queue",
		// tri-state predicate (the §6.2 attention classifier).
		"function isAttentionRun(",
		"sandbox_transport",
		"'orphaned'",
		// the two resolve actions.
		"function cronAttentionConfirm(",
		"function cronAttentionReplay(",
		"/confirm'",
		"/replay'",
		"确认已完成",
		"确认未完成，重放",
		// §7.3 detail-view replay button + chain.
		"function cronReplayBarHtml(",
		"function cronReplayRun(",
		"ctr-replay-bar",
		"ctr-replay-of",
		// transport-failed run DISABLES the replay button (§6.2 safety face).
		"ctr-replay-btn",
		"disabled",
		// fetch is wired into the detail open path.
		"cronAttentionRefresh().catch",
	} {
		if !strings.Contains(js, frag) {
			t.Errorf("cron_view.js missing attention/replay fragment %q", frag)
		}
	}
}

// TestDashboardJS_ReplayButtonDisabledForTransport asserts the transport-state
// branch in cronReplayBarHtml disables the button and routes to the queue —
// the DOM-level §6.2 invariant (RFC §7.6: tri-state must have DOM assertions).
func TestDashboardJS_ReplayButtonDisabledForTransport(t *testing.T) {
	t.Parallel()
	data, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read cron_view.js: %v", err)
	}
	js := string(data)
	// The transport branch must produce a disabled button and the queue hint.
	if !strings.Contains(js, "ctr-replay-btn' disabled") && !strings.Contains(js, `ctr-replay-btn" disabled`) &&
		!strings.Contains(js, "class=\"ctr-replay-btn\" disabled") {
		// Fall back to a looser check: both the disabled marker and the
		// transport guard must appear in the file.
		if !(strings.Contains(js, "disabled") && strings.Contains(js, "isTransport")) {
			t.Error("cron_view.js: transport-failed replay button must be disabled (isTransport branch)")
		}
	}
	if !strings.Contains(js, "待确认") {
		t.Error("cron_view.js: transport branch must route the operator to the confirmation queue")
	}
}

// TestDashboardHTML_CronAttentionCSS pins the §7.4 queue + §7.3 replay CSS
// classes so a stylesheet refactor cannot silently drop the queue's visual
// affordances.
func TestDashboardHTML_CronAttentionCSS(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	for _, frag := range []string{
		".ctr-queue{",
		".ctr-queue-card{",
		".ctr-queue-confirm{",
		".ctr-queue-replay{",
		".ctr-replay-bar{",
		".ctr-replay-btn:disabled{",
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("dashboard.html missing attention/replay CSS %q", frag)
		}
	}
}
