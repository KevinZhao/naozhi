package server

import (
	"regexp"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/turn"
)

// TestCheatsheetDashboardSlashCommandsAreRecognised pins the help panel's
// '斜杠命令' section to what sessionSend handles: every command listed there
// must be one turn.Parse recognises as bare input, and none of the IM-only
// commands (served by internal/dispatch/commands.go) may appear in it. This is
// a JS-to-Go contract the Playwright mock-server cannot check, because it
// never runs turn.Parse.
func TestCheatsheetDashboardSlashCommandsAreRecognised(t *testing.T) {
	js := readStaticAsset(t, "dashboard.js")
	const head = "{ section: '斜杠命令' },"
	start := strings.Index(js, head)
	if start < 0 {
		t.Fatalf("dashboard.js: %q not found in CHEATSHEET_ENTRIES", head)
	}
	section := js[start+len(head):]
	if end := strings.Index(section, "{ section:"); end >= 0 {
		section = section[:end]
	}

	keyRe := regexp.MustCompile(`keys: \['([^']+)'\]`)
	var cmds []string
	for _, m := range keyRe.FindAllStringSubmatch(section, -1) {
		cmds = append(cmds, m[1])
	}
	if len(cmds) < 3 {
		t.Fatalf("dashboard slash-command section lists %d commands %q, want at least 3", len(cmds), cmds)
	}
	for _, c := range cmds {
		cmd := turn.Parse(c)
		handled := (cmd.Kind == turn.CmdReset && cmd.Arg == "") || cmd.Kind == turn.CmdUrgentUsage || cmd.Kind == turn.CmdUrgent
		if !handled {
			t.Errorf("cheatsheet lists %q under '斜杠命令', but sessionSend sends it to the CLI as plain text", c)
		}
	}
	for _, imOnly := range []string{"/cd", "/pwd", "/project", "/cron", "/help", "/stop"} {
		if strings.Contains(section, "keys: ['"+imOnly) {
			t.Errorf("IM-only command %s is listed under the dashboard '斜杠命令' section", imOnly)
		}
	}
}
