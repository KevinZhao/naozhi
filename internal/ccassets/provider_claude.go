// Package ccassets implements the Claude Code asset provider for the dashboard
// asset browser (docs/rfc/cc-asset-browser.md): it scans ~/.claude and the
// workspace for skills/agents/commands/hooks/mcp/memory and serves raw files
// behind a path-traversal-safe whitelist. It holds the only knowledge of CC's
// on-disk layout and depends on the leaf package internal/assets, never the reverse.
package ccassets

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/assets"
)

// ClaudeProvider implements assets.Provider for the claude backend (stateless).
type ClaudeProvider struct{}

// NewClaudeProvider returns a ready provider.
func NewClaudeProvider() *ClaudeProvider { return &ClaudeProvider{} }

var _ assets.Provider = (*ClaudeProvider)(nil)

// Scan performs a full scan and returns the inventory. req.Kind, when set,
// narrows the returned Assets; Totals always reflects the full scan.
func (p *ClaudeProvider) Scan(req assets.ScanRequest) (*assets.Inventory, error) {
	inv := &assets.Inventory{Totals: map[string]int{}}

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

	pluginAssets, pluginInfos, err := p.scanPlugins(req.Home)
	if err != nil {
		return nil, err
	}
	inv.Assets = append(inv.Assets, pluginAssets...)
	inv.Plugins = pluginInfos

	inv.Assets = append(inv.Assets, scanMCP(req.Home)...)

	inv.Assets = append(inv.Assets, scanMemory(req.Home, req.RepoRoot)...)

	for i := range inv.Assets {
		inv.Totals[inv.Assets[i].Kind]++
	}

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

// scanPlugins scans each installed plugin's declared skills/commands dirs
// plus convention agents/ and hooks/hooks.json. Never recurses.
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
		// Security: a crafted installed_plugins.json with an installPath
		// outside home could otherwise enumerate arbitrary directories.
		if !isUnderHome(rec.InstallPath, home) {
			slog.Warn("ccassets: plugin installPath outside home, skipping", "id", id, "installPath", rec.InstallPath)
			continue
		}
		src := assets.Source{Kind: "plugin", Plugin: id}
		man := readPluginManifest(rec.InstallPath)

		var pa []assets.Asset
		for _, dir := range manifestDirsOr(man.Skills, "skills") {
			abs := filepath.Join(rec.InstallPath, dir)
			found, _ := scanSkillDir(abs, normalizeRel(dir)+"/", src)
			pa = append(pa, found...)
		}
		for _, dir := range manifestDirsOr(man.Commands, "commands") {
			abs := filepath.Join(rec.InstallPath, dir)
			found, _ := scanMarkdownDir(abs, "command", normalizeRel(dir)+"/", src)
			pa = append(pa, found...)
		}
		ag, _ := scanMarkdownDir(filepath.Join(rec.InstallPath, "agents"), "agent", "agents/", src)
		pa = append(pa, ag...)
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

// scanSkillDir reads each DIRECT subdirectory's SKILL.md frontmatter (never
// recurses). No readable SKILL.md → skipped; no frontmatter → name=<dir>.
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
		// readFrontmatter's os.Open is the existence check (no Stat per subdir).
		meta, err := readFrontmatter(skillPath)
		if err != nil {
			continue
		}
		name := meta.name
		if name == "" {
			name = dirName
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

// ReadRaw returns the raw bytes of req.Ref after validating its allowed root.
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
