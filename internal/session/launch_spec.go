package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

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
	}
	if hasSettingsOverrides(ov) && c.sessionSettingsDir != "" {
		ls.SettingsFile = filepath.Join(c.sessionSettingsDir, persist.KeyHash(key)+".json")
	}
	return ls
}

func hasSettingsOverrides(ov shim.SpawnOverlay) bool {
	return ov.CrossSessionInbound != "" || len(ov.PermissionsDeny) > 0
}

// writeSessionSettings writes the per-session settings file at path: the
// naozhi-owned settings file base (or {} when base is "" and the CLI loads
// ~/.claude/settings.json itself) plus ov's crossSessionInbound and
// permissions.deny. Rewritten on every spawn, so a base edit reaches the
// session on its next spawn, not live.
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
	if len(ov.PermissionsDeny) > 0 {
		perms, _ := doc["permissions"].(map[string]any)
		if perms == nil {
			perms = map[string]any{}
		}
		deny, _ := perms["deny"].([]any)
		for _, rule := range ov.PermissionsDeny {
			if !slices.Contains(deny, any(rule)) {
				deny = append(deny, rule)
			}
		}
		perms["deny"] = deny
		doc["permissions"] = perms
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := datadir.EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return osutil.WriteFileAtomic(path, data, 0o600)
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
