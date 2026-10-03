package server

import (
	"regexp"
	"strings"
	"testing"
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
