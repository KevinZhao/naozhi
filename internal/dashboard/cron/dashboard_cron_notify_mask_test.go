package cron

import (
	"strings"
	"testing"
)

// TestMaskNotifyChatID pins R243-SEC-7 (#789): the global cron.notify_default
// chat_id must never be surfaced verbatim in the list response. Before this
// fix the raw chat_id was sent to every authenticated dashboard user, leaking
// the notification target (e.g. a private Feishu open_id) cross-tenant in a
// multi-operator deployment.
//
// Contract:
//   - empty stays empty (omitempty path),
//   - any non-empty ID is masked so the full string is NOT a substring of the
//     output (no full-ID leak),
//   - long IDs keep a short prefix/suffix hint so the UI copy still works.
func TestMaskNotifyChatID(t *testing.T) {
	t.Parallel()

	if got := maskNotifyChatID(""); got != "" {
		t.Errorf("empty chat_id must stay empty, got %q", got)
	}

	longIDs := []string{
		"oc_0123456789abcdef",
		"C0123456789",
		"feishu-private-chat-target-987654321",
	}
	for _, id := range longIDs {
		got := maskNotifyChatID(id)
		if got == id {
			t.Errorf("maskNotifyChatID(%q) returned the ID unchanged — full-ID leak", id)
		}
		if strings.Contains(got, id) {
			t.Errorf("maskNotifyChatID(%q) = %q still contains the full ID", id, got)
		}
		if !strings.Contains(got, "…") {
			t.Errorf("maskNotifyChatID(%q) = %q dropped the prefix/suffix hint affordance", id, got)
		}
		// Hint must reveal at most a handful of characters from each end.
		r := []rune(id)
		if !strings.HasPrefix(got, string(r[:4])) {
			t.Errorf("maskNotifyChatID(%q) = %q lost the 4-rune prefix hint", id, got)
		}
	}

	// Short IDs (<=8 runes) carry no safe hint and must be fully masked.
	for _, id := range []string{"a", "oc_short", "12345678"} {
		got := maskNotifyChatID(id)
		if strings.ContainsAny(got, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") {
			t.Errorf("short ID %q must be fully masked, got %q (leaked characters)", id, got)
		}
	}
}
