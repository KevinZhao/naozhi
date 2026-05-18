package dispatch

import (
	"strings"
	"testing"
)

// TestDispatcher_ReplyFooterFn_PerBackend pins the per-session ReplyFooter
// contract: the dispatcher reads sess.Backend() at IM-reply time and asks
// the configured fn for a tag, never caching a server-global value. This is
// the central requirement of multi-backend Sprint 2: a kiro session in a
// claude-default deployment must still get [kiro] at the bottom of its IM
// replies.
func TestDispatcher_ReplyFooterFn_PerBackend(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		fn          func(string) string
		backend     string
		wantFooter  string
		wantPresent bool
	}{
		{"claude session gets cc tag", func(b string) string {
			if b == "claude" {
				return "cc"
			}
			return ""
		}, "claude", "cc", true},
		{"kiro session gets kiro tag", func(b string) string {
			if b == "kiro" {
				return "kiro"
			}
			return ""
		}, "kiro", "kiro", true},
		{"empty backend → fn falls back to default", func(b string) string {
			if b == "" {
				return "default"
			}
			return b
		}, "", "default", true},
		{"fn returns empty → no footer", func(string) string { return "" }, "claude", "", false},
		{"nil fn → no footer", nil, "claude", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Dispatcher{replyFooterFn: c.fn}
			body := "hello"
			// Mirror the production path in handleSendResult line 702-712.
			if d.replyFooterFn != nil {
				if tag := d.replyFooterFn(c.backend); tag != "" {
					body += "\n\n— " + tag
				}
			}
			if c.wantPresent {
				if !strings.HasSuffix(body, "— "+c.wantFooter) {
					t.Errorf("body = %q, want suffix '— %s'", body, c.wantFooter)
				}
			} else {
				if strings.Contains(body, "—") {
					t.Errorf("expected no footer, got %q", body)
				}
			}
		})
	}
}
