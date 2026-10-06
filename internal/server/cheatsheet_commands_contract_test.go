package server

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestCheatsheetDashboardSlashCommandsAreRecognised pins the help panel's
// '斜杠命令' section to what sessionSend handles: every command listed there,
// sent from the dashboard, must reset or be rejected rather than reach the CLI
// as a prompt, and none of the IM-only commands (served by
// internal/dispatch/commands.go) may be listed. This is a JS-to-Go contract the
// Playwright mock-server cannot check, because it never parses commands.
func TestCheatsheetDashboardSlashCommandsAreRecognised(t *testing.T) {
	js := readStaticAsset(t, "dashboard.js")
	head := regexp.MustCompile(`section:\s*['"]斜杠命令['"]`).FindStringIndex(js)
	if head == nil {
		t.Fatal("dashboard.js: section '斜杠命令' not found in CHEATSHEET_ENTRIES")
	}
	section := js[head[1]:]
	if end := regexp.MustCompile(`section:\s*['"]`).FindStringIndex(section); end != nil {
		section = section[:end[0]]
	}

	keyRe := regexp.MustCompile(`keys:\s*\[\s*['"]([^'"]+)['"]`)
	var cmds []string
	for _, m := range keyRe.FindAllStringSubmatch(section, -1) {
		cmds = append(cmds, m[1])
	}
	if len(cmds) < 3 {
		t.Fatalf("dashboard slash-command section lists %d commands %q, want at least 3", len(cmds), cmds)
	}
	for _, c := range cmds {
		name := strings.Fields(c)[0]
		for _, imOnly := range []string{"/cd", "/pwd", "/project", "/cron", "/help", "/stop"} {
			if strings.EqualFold(name, imOnly) {
				t.Errorf("IM-only command %s is listed under the dashboard '斜杠命令' section", c)
			}
		}
	}

	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	ws := h.ws()
	for i, c := range cmds {
		id := "c" + string(rune('a'+i))
		ws.send(id, c)
		if s := ws.ack(t, id); s != "reset" && s != "error" {
			t.Errorf("cheatsheet lists %q under '斜杠命令', but the dashboard send was %s: it reached the CLI as plain text", c, s)
		}
		h.waitEngineIdle()
	}
	turns.noMoreTurns(t)
}

// TestCheatsheetCronRowMatchesDispatchUsage pins the help panel's /cron row to
// what the IM dispatcher tells users: every subcommand in the "/cron <a|b|…>"
// synopses of its /help and /cron replies must appear in the row as a /cron
// invocation, and every flag in the /cron add usage reply must be listed too,
// so a new subcommand or flag cannot ship without the dashboard showing it.
func TestCheatsheetCronRowMatchesDispatchUsage(t *testing.T) {
	p := &mockPlatform{}
	handler := newTestDispatcher(newTestServerWithScheduler(p)).BuildHandler()
	replyTo := func(text string) string {
		t.Helper()
		before := len(p.allReplies())
		handler(context.Background(), platform.IncomingMessage{
			Platform: "test", EventID: "cheatsheet-" + text, ChatID: "chat1", Text: text,
		})
		replies := p.allReplies()
		if len(replies) != before+1 {
			t.Fatalf("%q: got %d replies, want 1", text, len(replies)-before)
		}
		return replies[before].Text
	}

	var subs []string
	synopsis := regexp.MustCompile(`/cron <([a-z]+(?:\|[a-z]+)+)>`)
	for _, cmd := range []string{"/help", "/cron"} {
		m := synopsis.FindStringSubmatch(replyTo(cmd))
		if m == nil {
			t.Fatalf("%s reply carries no \"/cron <sub|…>\" synopsis", cmd)
		}
		for _, sub := range strings.Split(m[1], "|") {
			if !slices.Contains(subs, sub) {
				subs = append(subs, sub)
			}
		}
	}
	if len(subs) < 3 {
		t.Fatalf("parsed /cron subcommands %q, want at least 3", subs)
	}
	addUsage := replyTo("/cron add")
	flags := regexp.MustCompile(`--[a-z][a-z-]*`).FindAllString(addUsage, -1)
	if len(flags) == 0 {
		t.Fatalf("/cron add usage %q lists no flags; update this test if that is intended", addUsage)
	}

	js := readStaticAsset(t, "dashboard.js")
	row := regexp.MustCompile(`keys:\s*\['/cron'\],\s*desc:\s*'([^']*)'`).FindStringSubmatch(js)
	if row == nil {
		t.Fatal("dashboard.js: CHEATSHEET_ENTRIES has no keys: ['/cron'] row")
	}
	for _, sub := range subs {
		re := regexp.MustCompile(`/cron (?:[a-z]+\|)*` + regexp.QuoteMeta(sub) + `\b`)
		if !re.MatchString(row[1]) {
			t.Errorf("cheatsheet /cron row %q does not list subcommand %q from the IM usage replies", row[1], sub)
		}
	}
	for _, f := range flags {
		if !strings.Contains(row[1], f) {
			t.Errorf("cheatsheet /cron row %q does not list flag %s (/cron add usage: %q)", row[1], f, addUsage)
		}
	}
}
