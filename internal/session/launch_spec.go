package session

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/shim"
)

// launchSpecFor turns the overlay's per-session launch fields into the
// LaunchSpec ArgvSpawnOptions renders. Shared by the spawn and the drift
// check: the settings path is a pure function of the key, so a surviving
// shim's argv rebuilds exactly. Pure; the spawn writes the file itself
// (writeSessionSettings).
func (c *spawnConfig) launchSpecFor(key string, ov shim.SpawnOverlay) LaunchSpec {
	ls := LaunchSpec{
		Tools:           ov.Tools,
		PermissionMode:  ov.PermissionMode,
		Name:            ov.Name,
		StrictMCPConfig: ov.StrictMCPConfig,
		MCPConfigFile:   ov.MCPConfig,
		// planner:main works in the state root and must stay read-only with
		// no MCP (RFC planner §4.1), so it gets no project sources; nor does
		// a scout session, whose rules are its own settings alone (§15.6).
		ProjectSettings: key != sessionkey.PlannerMainKey && !sessionkey.IsScoutKey(key),
	}
	if hasSettingsOverrides(ov) && c.sessionSettingsDir != "" {
		ls.SettingsFile = filepath.Join(c.sessionSettingsDir, persist.KeyHash(key)+".json")
	}
	return ls
}

func hasSettingsOverrides(ov shim.SpawnOverlay) bool {
	return ov.CrossSessionInbound != "" || len(ov.PermissionsDeny) > 0 || len(ov.PermissionsAllow) > 0 || ov.Ultracode || ov.PromptCacheTTL != ""
}

// writeSessionSettings writes the per-session settings file at path: the
// naozhi-owned settings file base (or {} when base is "" and the CLI loads
// ~/.claude/settings.json itself) plus ov's crossSessionInbound,
// permissions.deny / permissions.allow, ultracode and promptCacheTtl.
// Rewritten on every spawn, so a base edit reaches the session on its next
// spawn, not live.
func writeSessionSettings(path, base string, ov shim.SpawnOverlay) error {
	doc := map[string]any{}
	if base != "" {
		data, err := os.ReadFile(base)
		if err != nil {
			return fmt.Errorf("read base settings: %w", err)
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("parse base settings %s: %w", base, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
	}
	if ov.CrossSessionInbound != "" {
		doc["crossSessionInbound"] = ov.CrossSessionInbound
	}
	if ov.Ultracode {
		doc["ultracode"] = true
	}
	if ov.PromptCacheTTL != "" {
		doc["promptCacheTtl"] = ov.PromptCacheTTL
	}
	addPermissionRules(doc, "deny", ov.PermissionsDeny)
	addPermissionRules(doc, "allow", ov.PermissionsAllow)
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := datadir.EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return osutil.WriteFileAtomic(path, data, 0o600)
}

// addPermissionRules appends rules to doc's permissions.<list>, keeping the
// base file's own rules.
func addPermissionRules(doc map[string]any, list string, rules []string) {
	if len(rules) == 0 {
		return
	}
	perms, _ := doc["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	cur, _ := perms[list].([]any)
	for _, rule := range rules {
		if !slices.Contains(cur, any(rule)) {
			cur = append(cur, rule)
		}
	}
	perms[list] = cur
	doc["permissions"] = perms
}

// maxCLINameRunes caps a --name value; ListAgents shows it in a table.
const maxCLINameRunes = 64

// cliSessionName is the --name a session spawns with when the caller set
// none: its label, else a short name from the key. Dashboard and takeover
// chat IDs are random or path hashes, so those keys use the workspace's
// base name — what the CLI would name the session by itself.
func cliSessionName(key, label, workspace string) string {
	if n := cleanCLIName(label); n != "" {
		return n
	}
	parts := strings.Split(key, ":")
	if len(parts) != 4 { // cron:<id>, project:<name>:planner, scratch:<id>, ...
		return cleanCLIName(strings.Join(parts, "-"))
	}
	base := parts[2]
	if (parts[0] == sessionkey.DashboardPlatform || parts[0] == "local") && workspace != "" {
		base = filepath.Base(workspace)
	}
	if parts[3] != "general" {
		base += "-" + parts[3]
	}
	return cleanCLIName(base)
}

// settleResumedCLIName resets the defaulted --name of a resumed Claude spawn
// to resumedCLIName, overlay included so the drift side rebuilds it. Outside
// any lock: it reads the transcript tail.
func (r *Router) settleResumedCLIName(res *spawnReservation) {
	if res.opts.Name != "" || res.resumeID == "" || r.hist.claudeDir == "" ||
		cmp.Or(res.backendID, "claude") != "claude" || res.spawnOpts.SpawnOverlay == nil {
		return
	}
	held, aiTitle, err := claudefs.TranscriptTitles(claudefs.LocateSessionJSONL(r.hist.claudeDir, res.workspace, res.resumeID))
	if err != nil {
		slog.Debug("cli name: transcript titles unreadable", "key", res.key, "err", err)
	}
	name := resumedCLIName(res.spawnOpts.Name, cliSessionName(res.key, "", res.workspace), held, aiTitle)
	res.spawnOpts.Name, res.spawnOpts.SpawnOverlay.Name = name, name
}

// resumedCLIName is the --name of a resumed session whose caller set none.
// It re-passes the name the transcript holds (held: an earlier --name or a
// terminal /rename), as the CLI keeps a name only if it is given again — so a
// name, once given, stays and a later relabel does not rename the session.
// The exception is a held placeholder (the key-derived name) once the session
// has a title: def, the label-derived default, replaces it. A session holding
// no name keeps its aiTitle over the placeholder.
func resumedCLIName(def, placeholder, held, aiTitle string) string {
	held, aiTitle = cleanCLIName(held), cleanCLIName(aiTitle)
	switch {
	case held != "" && (held != placeholder || def == placeholder):
		return held
	case def == placeholder && aiTitle != "":
		return aiTitle
	}
	return def
}

// cleanCLIName folds whitespace and control runes to single spaces, drops
// leading dashes (argv-injection guard; BuildArgs drops such a value
// anyway) and caps the length.
func cleanCLIName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimLeft(strings.Join(strings.Fields(s), " "), "- ")
	if r := []rune(s); len(r) > maxCLINameRunes {
		s = string(r[:maxCLINameRunes])
	}
	return strings.TrimSpace(s)
}
