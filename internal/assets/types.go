// Package assets is the zero-dependency leaf package for the dashboard
// "installed assets" browser (RFC docs/rfc/cc-asset-browser.md). It holds
// only the pure DTO types and the Provider interface — nothing that imports
// other internal packages — so that both internal/cli/backend (which hangs
// a Provider off Profile) and internal/ccassets (which implements it) can
// depend on it without a cycle. See RFC §3.0 for the dependency rationale.
package assets

// Asset is a single installed extension item (a skill, agent, command, hook,
// MCP server, or memory file) surfaced read-only to the dashboard.
//
// Deliberately carries no absolute-path field: the absolute path is derived
// inside the provider from (Source + RelPath) and never serialised, so the
// browser cannot learn the server's filesystem layout (RFC §3.2 / §5).
type Asset struct {
	// Kind is one of: skill | agent | command | hook | mcp | memory.
	// A bare string (not an enum) by decision D1 — mirrors backend.ID and
	// keeps JSON trivial; spelling is guarded by tests.
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      Source `json:"source"`
	// RelPath is the path relative to the asset's own root. It is the stable
	// key the client echoes back via Ref to read the raw file (§4).
	RelPath string `json:"rel_path"`
	// Anchor locates a sub-object inside a multi-entry file: for hooks it is
	// the hook id, for mcp the server key; empty for one-file-one-asset kinds
	// (skill/agent/command/memory). Multiple hooks share one RelPath and are
	// disambiguated by Anchor (RFC decision C, §3.2).
	Anchor string `json:"anchor,omitempty"`
}

// Source classifies where an asset came from.
type Source struct {
	// Kind is one of: user | project | plugin | memory_project.
	//   user            ~/.claude/{skills,agents,...}
	//   project         <repoRoot>/.claude/...
	//   plugin          embedded in a plugin; Plugin names which one
	//   memory_project  ~/.claude/projects/<encoded>/memory/
	Kind string `json:"kind"`
	// Plugin is set only when Kind=="plugin": e.g. "ecc@ecc".
	Plugin string `json:"plugin,omitempty"`
	// Project is set only when Kind=="memory_project": the encoded project
	// directory name (e.g. "-home-ec2-user-workspace-naozhi").
	Project string `json:"project,omitempty"`
}

// PluginInfo describes one installed plugin for the Plugins view (§3.2 / G4).
// AssetCounts covers only this plugin's contributed assets, per kind.
type PluginInfo struct {
	ID          string         `json:"id"` // "ecc@ecc"
	Version     string         `json:"version"`
	Scope       string         `json:"scope"`
	Marketplace string         `json:"marketplace"`
	InstalledAt string         `json:"installed_at"`
	CommitSHA   string         `json:"commit_sha,omitempty"`
	AssetCounts map[string]int `json:"asset_counts"`
}

// Inventory is the full read-only snapshot of installed assets.
//
// Totals carries the per-kind count across ALL sources (user + project +
// plugin + memory), which is what the dashboard tab badges show. It is
// distinct from PluginInfo.AssetCounts (plugin-only) — decision B, §3.2.
type Inventory struct {
	Assets  []Asset        `json:"assets"`
	Plugins []PluginInfo   `json:"plugins,omitempty"`
	Totals  map[string]int `json:"totals"`
}

// Ref precisely locates one asset for the raw-read endpoint: Source + Kind +
// RelPath (+ Anchor). It deliberately does not use a slug (which would carry
// the global-dedup ambiguity of the ext/memory endpoint — §9.1). The server
// derives the absolute path from a Ref and re-validates it; the browser never
// sees an absolute path.
type Ref struct {
	Kind    string
	Source  Source
	RelPath string
	Anchor  string
}
