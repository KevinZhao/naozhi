// Package assets is the zero-dependency leaf package for the dashboard
// "installed assets" browser: pure DTO types plus the Provider interface, so
// internal/cli/backend and internal/ccassets can both depend on it without a
// cycle (docs/rfc/cc-asset-browser.md).
package assets

// Asset is one installed extension item surfaced read-only to the dashboard.
// It carries no absolute path: the provider derives it from (Source + RelPath)
// and never serialises it, so the browser cannot learn the filesystem layout.
type Asset struct {
	// Kind is one of: skill | agent | command | hook | mcp | memory.
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      Source `json:"source"`
	// RelPath is relative to the asset's own root; the client echoes it back via Ref.
	RelPath string `json:"rel_path"`
	// Anchor locates a sub-object inside a multi-entry file (hook id, mcp
	// server key); empty for one-file-one-asset kinds.
	Anchor string `json:"anchor,omitempty"`
}

// Source classifies where an asset came from.
type Source struct {
	// Kind is one of: user | project | plugin | memory_project.
	Kind string `json:"kind"`
	// Plugin is set only when Kind=="plugin", e.g. "ecc@ecc".
	Plugin string `json:"plugin,omitempty"`
	// Project is set only when Kind=="memory_project": the encoded project dir name.
	Project string `json:"project,omitempty"`
}

// PluginInfo describes one installed plugin; AssetCounts covers only this
// plugin's contributed assets, per kind.
type PluginInfo struct {
	ID          string         `json:"id"`
	Version     string         `json:"version"`
	Scope       string         `json:"scope"`
	Marketplace string         `json:"marketplace"`
	InstalledAt string         `json:"installed_at"`
	CommitSHA   string         `json:"commit_sha,omitempty"`
	AssetCounts map[string]int `json:"asset_counts"`
}

// Inventory is the full read-only snapshot. Totals counts per kind across ALL
// sources (what the tab badges show), distinct from PluginInfo.AssetCounts.
type Inventory struct {
	Assets  []Asset        `json:"assets"`
	Plugins []PluginInfo   `json:"plugins,omitempty"`
	Totals  map[string]int `json:"totals"`
}

// Ref locates one asset for the raw-read endpoint (deliberately not a slug). The
// server derives and re-validates the absolute path; the browser never sees one.
type Ref struct {
	Kind    string
	Source  Source
	RelPath string
	Anchor  string
}
