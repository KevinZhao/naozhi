package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/naozhi/naozhi/internal/claudefs"
)

// localSettingsMarker stands for "this process runs on `--setting-sources user`",
// i.e. cc's own ~/.claude/settings.json rather than a naozhi-owned file.
var localSettingsMarker = ""

// usableSettingsFile returns the settings path BuildArgs may pass to
// `--settings`, or "" to stay on `--setting-sources user`.
//
// The path must be absolute with no leading '-': a relative path makes cc
// silently re-read the operator's settings, and a leading '-' is argv injection.
func usableSettingsFile(path string) string {
	if path == "" || !filepath.IsAbs(path) || strings.HasPrefix(path, "-") {
		return ""
	}
	return path
}

// modelCache memoises one settings file's model list, invalidated when the file
// changes on disk so `naozhi models sync -write` reaches a long-lived process
// without a restart.
type modelCache struct {
	mu     sync.Mutex
	stamp  string
	models []ModelInfo
}

// AvailableModels returns the models the settings file this process spawned with
// allows, in file order. Nil when the file names none, which lets the dashboard
// fall through to its configured and observed tiers.
//
// This is the optional facet Process.AvailableModels surfaces; for stream-json
// the list is a settings key rather than something the agent reports.
func (p *ClaudeProtocol) AvailableModels() []ModelInfo {
	path := ""
	if sp := p.settingsPath.Load(); sp != nil {
		path = *sp
	}
	if path == "" {
		dir := claudefs.DefaultDir()
		if dir == "" {
			return nil
		}
		path = filepath.Join(dir, "settings.json")
	}
	return p.models.get(path)
}

// get returns the cached list for path, re-reading when the file's size or
// mtime moved.
func (c *modelCache) get(path string) []ModelInfo {
	stamp := fileStamp(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stamp == stamp && stamp != "" {
		return c.models
	}
	c.stamp = stamp
	c.models = readAvailableModels(path)
	return c.models
}

// fileStamp is a cheap change token for path; "" when it cannot be stat'ed,
// which forces a re-read rather than trusting a stale list.
func fileStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fi.ModTime().UTC().Format("20060102150405.000000000") + ":" +
		strconv.FormatInt(fi.Size(), 10)
}

// readAvailableModels extracts the settings file's availableModels list. Any
// read or parse failure yields nil: a malformed settings file must not be
// reported as "this backend offers no models at all".
func readAvailableModels(path string) []ModelInfo {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		AvailableModels []string `json:"availableModels"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	out := make([]ModelInfo, 0, len(doc.AvailableModels))
	for _, id := range doc.AvailableModels {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, ModelInfo{ID: id})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
