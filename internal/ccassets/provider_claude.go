// Package ccassets implements the Claude Code asset provider for the dashboard
// asset browser (RFC docs/rfc/cc-asset-browser.md). It scans ~/.claude and the
// current workspace for installed skills/agents/commands/hooks/mcp/memory and
// serves their raw files behind a path-traversal-safe whitelist.
//
// This package holds the only knowledge of CC's on-disk layout; the public
// surface is just assets.Provider (RFC §3.1/§3.2). It depends on the
// zero-dependency leaf package internal/assets, never the reverse.
//
// P0 scope (RFC §7): user-level + project-level skills only, no plugin scan,
// no cache. Later phases extend Scan to plugins/agents/commands/hooks/mcp/
// memory and add the event-driven cache (§3.4).
package ccassets

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/assets"
)

// ClaudeProvider implements assets.Provider for the claude backend.
// Stateless w.r.t. environment: Home/RepoRoot arrive per-call (RFC §3.1).
type ClaudeProvider struct{}

// NewClaudeProvider returns a ready provider. No configuration: all paths are
// derived from the per-call ScanRequest/RawRequest.
func NewClaudeProvider() *ClaudeProvider { return &ClaudeProvider{} }

// compile-time assertion that we satisfy the interface.
var _ assets.Provider = (*ClaudeProvider)(nil)

// Scan performs a full scan and returns the inventory. req.Kind, when set,
// narrows the returned Assets (handler-side concern, but honoured here for the
// direct-caller path); Totals always reflects the full scan (D4/D5).
func (p *ClaudeProvider) Scan(req assets.ScanRequest) (*assets.Inventory, error) {
	inv := &assets.Inventory{Totals: map[string]int{}}

	// 1) User-level + project-level skills (RFC §3.2 step 1).
	for _, s := range []struct{ kind, root, prefix string }{
		{"user", skillRoot(req.Home, req.RepoRoot, "user"), "skills/"},
		{"project", skillRoot(req.Home, req.RepoRoot, "project"), ".claude/skills/"},
	} {
		if s.root == "" {
			continue
		}
		found, err := scanSkillDir(s.root, s.prefix, assets.Source{Kind: s.kind})
		if err != nil {
			return nil, err
		}
		inv.Assets = append(inv.Assets, found...)
	}

	// 1b) User-level agents / commands (convention dirs under ~/.claude).
	if req.Home != "" {
		ua := assets.Source{Kind: "user"}
		ag, err := scanMarkdownDir(filepath.Join(req.Home, "agents"), "agent", "agents/", ua)
		if err != nil {
			slog.Warn("ccassets: failed to scan user agents dir", "dir", filepath.Join(req.Home, "agents"), "err", err)
		}
		cmd, err := scanMarkdownDir(filepath.Join(req.Home, "commands"), "command", "commands/", ua)
		if err != nil {
			slog.Warn("ccassets: failed to scan user commands dir", "dir", filepath.Join(req.Home, "commands"), "err", err)
		}
		inv.Assets = append(inv.Assets, ag...)
		inv.Assets = append(inv.Assets, cmd...)
	}

	// 2) Plugin-embedded assets (RFC §3.2 steps 2–3, 6).
	pluginAssets, pluginInfos, err := p.scanPlugins(req.Home)
	if err != nil {
		return nil, err
	}
	inv.Assets = append(inv.Assets, pluginAssets...)
	inv.Plugins = pluginInfos

	// 3) MCP servers (user-level only, D3).
	inv.Assets = append(inv.Assets, scanMCP(req.Home)...)

	// 4) Memory for the current workspace only (D6 + evaluation D).
	inv.Assets = append(inv.Assets, scanMemory(req.Home, req.RepoRoot)...)

	// 5) Totals across all sources; plugin AssetCounts already populated.
	for i := range inv.Assets {
		inv.Totals[inv.Assets[i].Kind]++
	}

	// 6) Narrow returned Assets if a kind filter was requested (Totals unchanged).
	if req.Kind != "" {
		filtered := inv.Assets[:0:0]
		for _, a := range inv.Assets {
			if a.Kind == req.Kind {
				filtered = append(filtered, a)
			}
		}
		inv.Assets = filtered
	}
	return inv, nil
}

// scanPlugins reads installed_plugins.json and, for each plugin, scans its
// declared skills/commands dirs (plugin.json) plus convention agents/ and
// hooks/hooks.json. Returns the assets and per-plugin PluginInfo (with
// AssetCounts). Never recurses into plugin dirs (RFC §1.2-3).
func (p *ClaudeProvider) scanPlugins(home string) ([]assets.Asset, []assets.PluginInfo, error) {
	if home == "" {
		return nil, nil, nil
	}
	ip, err := readInstalledPlugins(home)
	if err != nil {
		return nil, nil, err
	}
	if ip == nil {
		return nil, nil, nil
	}
	marketplaces := readMarketplaces(home)

	var allAssets []assets.Asset
	var infos []assets.PluginInfo
	for id, recs := range ip.Plugins {
		if len(recs) == 0 {
			continue
		}
		rec := recs[0]
		src := assets.Source{Kind: "plugin", Plugin: id}
		man := readPluginManifest(rec.InstallPath)

		var pa []assets.Asset
		// skills: plugin.json dirs, fallback to convention "skills/".
		for _, dir := range manifestDirsOr(man.Skills, "skills") {
			abs := filepath.Join(rec.InstallPath, dir)
			found, _ := scanSkillDir(abs, normalizeRel(dir)+"/", src)
			pa = append(pa, found...)
		}
		// commands: plugin.json dirs, fallback to convention "commands/".
		for _, dir := range manifestDirsOr(man.Commands, "commands") {
			abs := filepath.Join(rec.InstallPath, dir)
			found, _ := scanMarkdownDir(abs, "command", normalizeRel(dir)+"/", src)
			pa = append(pa, found...)
		}
		// agents: convention dir only (manifest forbids declaring, §1.2-2).
		ag, _ := scanMarkdownDir(filepath.Join(rec.InstallPath, "agents"), "agent", "agents/", src)
		pa = append(pa, ag...)
		// hooks: convention hooks/hooks.json, expanded per entry (D2).
		pa = append(pa, scanHooksJSON(filepath.Join(rec.InstallPath, "hooks", "hooks.json"), "hooks/hooks.json", src)...)

		allAssets = append(allAssets, pa...)

		counts := map[string]int{}
		for _, a := range pa {
			counts[a.Kind]++
		}
		infos = append(infos, assets.PluginInfo{
			ID:          id,
			Version:     rec.Version,
			Scope:       rec.Scope,
			Marketplace: marketplaceLabel(id, marketplaces),
			InstalledAt: rec.InstalledAt,
			CommitSHA:   shortSHA(rec.GitCommitSHA),
			AssetCounts: counts,
		})
	}
	return allAssets, infos, nil
}

// scanSkillDir walks the DIRECT subdirectories of root, reading each
// <name>/SKILL.md's frontmatter. Never recurses (RFC §1.2-3). A subdir without
// a readable SKILL.md is skipped; a SKILL.md without frontmatter degrades to
// name=<dir>, description="" (§1.2-6).
func scanSkillDir(root, relPrefix string, src assets.Source) ([]assets.Asset, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []assets.Asset
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		dirName := ent.Name()
		skillPath := filepath.Join(root, dirName, "SKILL.md")
		// readFrontmatter's os.Open IS the existence check: a missing
		// SKILL.md (or a directory named SKILL.md) returns an error here, so
		// we skip without a separate os.Stat syscall per subdir (R220123-
		// PERF-4). A present-but-frontmatter-less file returns nil and we
		// degrade to name=<dir> below (§1.2-6).
		meta, err := readFrontmatter(skillPath)
		if err != nil {
			continue // no readable SKILL.md in this subdir — not a skill
		}
		name := meta.name
		if name == "" {
			name = dirName // degrade (§1.2-6)
		}
		out = append(out, assets.Asset{
			Kind:        "skill",
			Name:        name,
			Description: meta.description,
			Source:      src,
			RelPath:     relPrefix + dirName + "/SKILL.md",
		})
	}
	return out, nil
}

// ReadRaw returns the raw bytes of the asset named by req.Ref, after deriving
// and validating its allowed root (RFC §5). Reads are capped at maxRawBytes.
func (p *ClaudeProvider) ReadRaw(req assets.RawRequest) ([]byte, error) {
	root, rel, err := rootForRef(req.Home, req.RepoRoot, req.Ref)
	if err != nil {
		return nil, err
	}
	resolved, err := resolveUnder(root, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, assets.ErrNotFound
		}
		return nil, err
	}
	raw, err := readCapped(resolved, maxRawBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, assets.ErrNotFound
		}
		return nil, err
	}
	return raw, nil
}
