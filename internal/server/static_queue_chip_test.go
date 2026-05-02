package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_QueueChipReplacesToast pins the migration of the
// "消息已排队" signal from a top-of-screen toast to an inline chip on the
// optimistic user bubble. The toast form was noisy on mobile (it covered
// the header) and detached from the message it described; the chip binds
// to the bubble so it vanishes naturally when the real "user" event
// replaces the optimistic DOM node — no separate lifecycle timer needed.
func TestDashboardJS_QueueChipReplacesToast(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Negative: the old toast string must not reappear. Pinning the exact
	// wording catches a future revert or copy-paste that reintroduces the
	// noise channel.
	if strings.Contains(js, "showToast('消息已排队") {
		t.Error("queued-send notice must not use showToast — migrate to the inline .msg-queued-chip on the optimistic bubble")
	}

	// Positive: the chip injection path exists and selects the last
	// optimistic user bubble (not just any .event.user, which would
	// mis-target the previous turn's message after a fast double-send).
	if !strings.Contains(js, "#events-scroll .event.user.optimistic-msg:last-of-type .event-content") {
		t.Error("onSendAck(queued) must target the last optimistic-msg inside events-scroll so the chip binds to the message we just sent")
	}
	if !strings.Contains(js, "chip.className = 'msg-queued-chip';") {
		t.Error("onSendAck(queued) must create a .msg-queued-chip element so the CSS rule in dashboard.html applies")
	}
	if !strings.Contains(js, "chip.textContent = '排队中…';") {
		t.Error("queued chip text must be '排队中…' so future i18n work can find all user-visible strings by search")
	}
	// Idempotency: a second send_ack for the same bubble (server retries,
	// duplicate ack) must not append a second chip. The existing check
	// uses querySelector('.msg-queued-chip') on the bubble as the guard.
	if !strings.Contains(js, "!lastOpt.querySelector('.msg-queued-chip')") {
		t.Error("onSendAck(queued) must guard against duplicate chip injection when the same optimistic bubble is acked twice")
	}
}

// TestDashboardHTML_QueueChipStyled pins the CSS rule that backs the
// inline queued indicator. The JS side creates elements with className
// "msg-queued-chip" inside .event.user.optimistic-msg; without the style
// rule those elements would render as full-width block divs and visually
// regress the bubble layout.
func TestDashboardHTML_QueueChipStyled(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)
	if !strings.Contains(css, ".event.user.optimistic-msg .msg-queued-chip{") {
		t.Error("dashboard.html must define .event.user.optimistic-msg .msg-queued-chip — backs the inline queued indicator that replaces the '消息已排队' toast")
	}
	// The rule must at minimum be inline (chip, not block) and muted (this
	// is a passive signal, not primary content). Pin those two traits so
	// a future CSS refactor doesn't silently turn the chip into a giant
	// red badge by losing the color/display tokens.
	chipRuleIdx := strings.Index(css, ".event.user.optimistic-msg .msg-queued-chip{")
	if chipRuleIdx < 0 {
		return
	}
	end := chipRuleIdx + 300
	if end > len(css) {
		end = len(css)
	}
	ruleBlock := css[chipRuleIdx:end]
	if !strings.Contains(ruleBlock, "display:inline-block") {
		t.Error(".msg-queued-chip must be inline-block — block would push the bubble width to full and break the right-aligned user message layout")
	}
	if !strings.Contains(ruleBlock, "color:var(--nz-text-mute)") {
		t.Error(".msg-queued-chip must use --nz-text-mute — this is a passive signal, not primary message content")
	}
}
