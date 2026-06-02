package ccassets

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// installedPlugins mirrors ~/.claude/plugins/installed_plugins.json (version 2).
// Each plugin id maps to a slice of install records (one per scope); we use the
// first record. RFC §1.2-1: installPath already points at the versioned dir.
type installedPlugins struct {
	Version int                           `json:"version"`
	Plugins map[string][]pluginInstallRec `json:"plugins"`
}

type pluginInstallRec struct {
	Scope        string `json:"scope"`
	InstallPath  string `json:"installPath"`
	Version      string `json:"version"`
	InstalledAt  string `json:"installedAt"`
	GitCommitSHA string `json:"gitCommitSha"`
}

// pluginManifest mirrors <installPath>/.claude-plugin/plugin.json. Only the
// component-path fields matter here. skills/commands are arrays of directory
// paths (RFC §1.2-2); agents/hooks are NOT declared (convention dirs).
type pluginManifest struct {
	Name     string   `json:"name"`
	Skills   []string `json:"skills"`
	Commands []string `json:"commands"`
}

type marketplaceSource struct {
	Source struct {
		Source string `json:"source"`
		Repo   string `json:"repo"`
	} `json:"source"`
}

// readInstalledPlugins parses the install manifest. Missing file => nil, nil
// (no plugins installed is not an error).
func readInstalledPlugins(home string) (*installedPlugins, error) {
	path := filepath.Join(home, "plugins", "installed_plugins.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var ip installedPlugins
	if err := json.Unmarshal(data, &ip); err != nil {
		return nil, nil // malformed manifest: degrade to "no plugins"
	}
	return &ip, nil
}

// readMarketplaces parses known_marketplaces.json into repo lookups. Best
// effort: a missing/bad file yields an empty (non-nil) map.
func readMarketplaces(home string) map[string]string {
	out := map[string]string{}
	path := filepath.Join(home, "plugins", "known_marketplaces.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var raw map[string]marketplaceSource
	if err := json.Unmarshal(data, &raw); err != nil {
		return out
	}
	for name, src := range raw {
		if src.Source.Repo != "" {
			out[name] = src.Source.Source + ":" + src.Source.Repo
		}
	}
	return out
}

// readPluginManifest parses <installPath>/.claude-plugin/plugin.json. Missing
// or malformed manifest => empty manifest (caller falls back to convention).
func readPluginManifest(installPath string) pluginManifest {
	path := filepath.Join(installPath, ".claude-plugin", "plugin.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return pluginManifest{}
	}
	var m pluginManifest
	_ = json.Unmarshal(data, &m)
	return m
}

// marketplaceLabel returns a human label for a plugin's marketplace: the repo
// from known_marketplaces if available (e.g. "github:affaan-m/..."), else the
// bare marketplace name parsed from the plugin id "<name>@<mp>".
func marketplaceLabel(pluginID string, repos map[string]string) string {
	mp := ""
	if i := strings.LastIndex(pluginID, "@"); i >= 0 {
		mp = pluginID[i+1:]
	}
	if repo, ok := repos[mp]; ok {
		return repo
	}
	return mp
}

// manifestDirsOr returns the manifest-declared directories, or a single
// fallback convention dir when the manifest omitted the field (RFC §1.2-2:
// simple plugins like gopls-lsp have no skills/commands key).
func manifestDirsOr(declared []string, fallback string) []string {
	if len(declared) == 0 {
		return []string{fallback}
	}
	return declared
}

// normalizeRel strips a leading "./" and trailing "/" from a manifest dir path
// so it can be used as a clean RelPath display prefix segment.
func normalizeRel(dir string) string {
	dir = strings.TrimPrefix(dir, "./")
	dir = strings.TrimRight(dir, "/")
	return dir
}

// shortSHA truncates a git commit sha to 7 chars for display; empty stays empty.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
