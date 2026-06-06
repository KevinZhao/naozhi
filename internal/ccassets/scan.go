package ccassets

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/assets"
	"github.com/naozhi/naozhi/internal/discovery"
)

// scanMarkdownDir scans the direct *.md files in dir (non-recursive), reading
// each file's frontmatter. Used for agents/commands (one .md = one asset).
// kind is "agent" or "command". For commands the name comes from the filename
// (commands have no `name:` frontmatter, §1.2-5); for agents the frontmatter
// name wins, falling back to the filename.
func scanMarkdownDir(dir, kind, relPrefix string, src assets.Source) ([]assets.Asset, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []assets.Asset
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".md") {
			continue
		}
		base := strings.TrimSuffix(ent.Name(), ".md")
		meta, _ := readFrontmatter(filepath.Join(dir, ent.Name()))
		name := meta.name
		if name == "" || kind == "command" {
			name = base // command: filename is canonical; agent: degrade
		}
		out = append(out, assets.Asset{
			Kind:        kind,
			Name:        name,
			Description: meta.description,
			Source:      src,
			RelPath:     relPrefix + ent.Name(),
		})
	}
	return out, nil
}

// hooksFile mirrors the subset of a hooks.json we surface: events map to
// arrays of entries, each with an id + description (§1.2 / decision D2).
type hooksFile struct {
	Hooks map[string][]hookEntry `json:"hooks"`
}

type hookEntry struct {
	ID          string `json:"id"`
	Matcher     string `json:"matcher"`
	Description string `json:"description"`
}

// scanHooksJSON parses one hooks.json and expands each hook entry to its own
// Asset (decision D2). All entries share the same RelPath; Anchor=hook id
// disambiguates them. A missing file yields nothing.
func scanHooksJSON(path, relPath string, src assets.Source) []assets.Asset {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var hf hooksFile
	if err := json.Unmarshal(data, &hf); err != nil {
		slog.Warn("ccassets: hooks.json parse failed", "path", path, "err", err)
		return nil
	}
	var out []assets.Asset
	for event, entries := range hf.Hooks {
		for _, e := range entries {
			name := e.ID
			if name == "" {
				name = e.Matcher + ":" + event // degrade
			}
			out = append(out, assets.Asset{
				Kind:        "hook",
				Name:        name,
				Description: e.Description,
				Source:      src,
				RelPath:     relPath,
				Anchor:      e.ID,
			})
		}
	}
	return out
}

// mcpFile mirrors ~/.claude/.mcp.json: {mcpServers: {<key>: {command, args}}}.
type mcpFile struct {
	MCPServers map[string]mcpServer `json:"mcpServers"`
}

type mcpServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Type    string   `json:"type"`
	URL     string   `json:"url"`
}

// scanMCP parses ~/.claude/.mcp.json; each server becomes one Asset
// (Source=user, Anchor=server key). Missing file yields nothing (D3).
func scanMCP(home string) []assets.Asset {
	path := filepath.Join(home, ".mcp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var mf mcpFile
	if err := json.Unmarshal(data, &mf); err != nil {
		return nil
	}
	var out []assets.Asset
	for key, srv := range mf.MCPServers {
		desc := srv.Command
		if srv.URL != "" {
			desc = srv.Type + " " + srv.URL
		} else if len(srv.Args) > 0 {
			desc = strings.TrimSpace(srv.Command + " " + strings.Join(srv.Args, " "))
		}
		out = append(out, assets.Asset{
			Kind:        "mcp",
			Name:        key,
			Description: desc,
			Source:      assets.Source{Kind: "user"},
			RelPath:     ".mcp.json",
			Anchor:      key,
		})
	}
	return out
}

// encodeProjectDir maps an absolute repo root to Claude's projects/<encoded>
// directory name. Delegates to discovery.ClaudeProjectSlug which is the single
// source of truth for the encoding (strips control bytes, "/" → "-").
// R20260603-CODE-2.
func encodeProjectDir(repoRoot string) string {
	if repoRoot == "" {
		return ""
	}
	return discovery.ClaudeProjectSlug(repoRoot)
}

// scanMemory scans ONLY the current repoRoot's project memory dir
// (~/.claude/projects/<encoded>/memory/*.md) — not all projects (§3.2 step 5,
// evaluation D). Empty repoRoot yields nothing.
func scanMemory(home, repoRoot string) []assets.Asset {
	if repoRoot == "" || home == "" {
		return nil
	}
	encoded := encodeProjectDir(repoRoot)
	dir := filepath.Join(home, "projects", encoded, "memory")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	src := assets.Source{Kind: "memory_project", Project: encoded}
	var out []assets.Asset
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".md") {
			continue
		}
		meta, _ := readFrontmatter(filepath.Join(dir, ent.Name()))
		name := meta.name
		if name == "" {
			name = strings.TrimSuffix(ent.Name(), ".md")
		}
		out = append(out, assets.Asset{
			Kind:        "memory",
			Name:        name,
			Description: meta.description,
			Source:      src,
			RelPath:     ent.Name(),
		})
	}
	return out
}
