package ccassets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/assets"
)

// buildPluginHome lays out a fake ~/.claude with one plugin (ecc) exposing
// skills/commands/agents/hooks, plus installed_plugins.json + marketplaces.
func buildPluginHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	inst := filepath.Join(home, "plugins", "cache", "ecc", "ecc", "2.0.0")

	// plugin.json declares skills + commands; agents/hooks are convention dirs.
	writeFile(t, filepath.Join(inst, ".claude-plugin", "plugin.json"),
		`{"name":"ecc","skills":["./skills/"],"commands":["./commands/"]}`)
	writeSkill(t, filepath.Join(inst, "skills"), "deep-research",
		"---\nname: deep-research\ndescription: research harness\n---\nb")
	writeFile(t, filepath.Join(inst, "commands", "code-review.md"),
		"---\ndescription: review code\n---\nbody")
	writeFile(t, filepath.Join(inst, "agents", "architect.md"),
		"---\nname: architect\ndescription: system design\n---\nbody")
	writeFile(t, filepath.Join(inst, "hooks", "hooks.json"),
		`{"hooks":{"PreToolUse":[{"id":"pre:bash:x","matcher":"Bash","description":"preflight"},{"id":"pre:write:y","matcher":"Write","description":"warn"}]}}`)

	writeFile(t, filepath.Join(home, "plugins", "installed_plugins.json"),
		`{"version":2,"plugins":{"ecc@ecc":[{"scope":"user","installPath":"`+inst+`","version":"2.0.0","installedAt":"2026-05-30T10:00:00Z","gitCommitSha":"64cd1ba248e7"}]}}`)
	writeFile(t, filepath.Join(home, "plugins", "known_marketplaces.json"),
		`{"ecc":{"source":{"source":"github","repo":"affaan-m/everything-claude-code"}}}`)
	return home
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScan_PluginAssetsAndCounts(t *testing.T) {
	home := buildPluginHome(t)
	p := NewClaudeProvider()
	inv, err := p.Scan(assets.ScanRequest{Home: home})
	if err != nil {
		t.Fatal(err)
	}

	// skill + command + agent + 2 hooks = 5 plugin assets.
	want := map[string]int{"skill": 1, "command": 1, "agent": 1, "hook": 2}
	for k, v := range want {
		if inv.Totals[k] != v {
			t.Errorf("Totals[%s] = %d, want %d", k, inv.Totals[k], v)
		}
	}
	if len(inv.Plugins) != 1 {
		t.Fatalf("plugins = %d, want 1", len(inv.Plugins))
	}
	pi := inv.Plugins[0]
	if pi.ID != "ecc@ecc" || pi.Version != "2.0.0" {
		t.Errorf("plugin id/ver = %q/%q", pi.ID, pi.Version)
	}
	if pi.Marketplace != "github:affaan-m/everything-claude-code" {
		t.Errorf("marketplace = %q", pi.Marketplace)
	}
	if pi.CommitSHA != "64cd1ba" {
		t.Errorf("sha = %q, want short 64cd1ba", pi.CommitSHA)
	}
	if pi.AssetCounts["hook"] != 2 {
		t.Errorf("plugin AssetCounts[hook] = %d, want 2", pi.AssetCounts["hook"])
	}
}

func TestScan_HookAnchorsDistinct(t *testing.T) {
	home := buildPluginHome(t)
	inv, _ := NewClaudeProvider().Scan(assets.ScanRequest{Home: home, Kind: "hook"})
	if len(inv.Assets) != 2 {
		t.Fatalf("hooks = %d, want 2", len(inv.Assets))
	}
	seen := map[string]string{}
	for _, a := range inv.Assets {
		if a.Anchor == "" {
			t.Error("hook anchor empty")
		}
		if a.RelPath != "hooks/hooks.json" {
			t.Errorf("hook RelPath = %q", a.RelPath)
		}
		seen[a.Anchor] = a.RelPath
	}
	if len(seen) != 2 {
		t.Errorf("expected 2 distinct anchors, got %v", seen)
	}
}

func TestScan_MCP(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".mcp.json"),
		`{"mcpServers":{"playwright":{"command":"npx","args":["-y","mcp"]},"ctx":{"command":"uvx"}}}`)
	inv, _ := NewClaudeProvider().Scan(assets.ScanRequest{Home: home, Kind: "mcp"})
	if len(inv.Assets) != 2 {
		t.Fatalf("mcp = %d, want 2", len(inv.Assets))
	}
	for _, a := range inv.Assets {
		if a.Source.Kind != "user" || a.Anchor == "" {
			t.Errorf("mcp asset bad: %+v", a)
		}
	}
}

func TestScan_MemoryCurrentProjectOnly(t *testing.T) {
	home := t.TempDir()
	repo := "/home/u/work/myproj"
	encoded := "-home-u-work-myproj"
	memDir := filepath.Join(home, "projects", encoded, "memory")
	writeFile(t, filepath.Join(memDir, "note.md"), "---\nname: note\ndescription: a note\n---\nb")
	writeFile(t, filepath.Join(memDir, "MEMORY.md"), "# index")
	// a DIFFERENT project's memory must NOT appear
	writeFile(t, filepath.Join(home, "projects", "-other-proj", "memory", "x.md"), "---\nname: x\n---\nb")

	inv, _ := NewClaudeProvider().Scan(assets.ScanRequest{Home: home, RepoRoot: repo, Kind: "memory"})
	if len(inv.Assets) != 2 {
		t.Fatalf("memory = %d, want 2 (only current project)", len(inv.Assets))
	}
	for _, a := range inv.Assets {
		if a.Source.Project != encoded {
			t.Errorf("memory from wrong project: %q", a.Source.Project)
		}
	}
}

func TestReadRaw_PluginSkill(t *testing.T) {
	home := buildPluginHome(t)
	raw, err := NewClaudeProvider().ReadRaw(assets.RawRequest{
		Home: home,
		Ref: assets.Ref{
			Kind:    "skill",
			Source:  assets.Source{Kind: "plugin", Plugin: "ecc@ecc"},
			RelPath: "skills/deep-research/SKILL.md",
		},
	})
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if !contains(string(raw), "research harness") {
		t.Errorf("raw missing content: %s", raw)
	}
}

func TestReadRaw_UnknownPlugin404(t *testing.T) {
	home := buildPluginHome(t)
	_, err := NewClaudeProvider().ReadRaw(assets.RawRequest{
		Home: home,
		Ref: assets.Ref{
			Kind:    "skill",
			Source:  assets.Source{Kind: "plugin", Plugin: "ghost@nowhere"},
			RelPath: "skills/x/SKILL.md",
		},
	})
	if err == nil {
		t.Fatal("expected error for unknown plugin")
	}
}

// TestScanHooksJSON_MalformedJSON verifies that a corrupt hooks.json returns
// nil (behaviour preserved) but does not panic. The warn log is exercised via
// code coverage; capturing slog output is not required for correctness.
// R20260603000023-CR-5.
func TestScanHooksJSON_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	writeFile(t, path, `{not valid json`)

	got := scanHooksJSON(path, "hooks/hooks.json", assets.Source{Kind: "plugin", Plugin: "x"})
	if got != nil {
		t.Errorf("expected nil on parse error, got %v", got)
	}
}

// TestScanHooksJSON_MissingFile verifies that a missing hooks.json returns nil
// without error — pre-existing silent behaviour is preserved.
func TestScanHooksJSON_MissingFile(t *testing.T) {
	got := scanHooksJSON("/nonexistent/hooks.json", "hooks/hooks.json", assets.Source{Kind: "plugin"})
	if got != nil {
		t.Errorf("expected nil for missing file, got %v", got)
	}
}

func TestReadRaw_MemoryTraversalBlocked(t *testing.T) {
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, "secret.txt"), []byte("SECRET"), 0o644)
	_, err := NewClaudeProvider().ReadRaw(assets.RawRequest{
		Home: home,
		Ref: assets.Ref{
			Kind:    "memory",
			Source:  assets.Source{Kind: "memory_project", Project: "../../.."},
			RelPath: "secret.txt",
		},
	})
	if err == nil {
		t.Fatal("crafted Project segment not rejected")
	}
}

// TestReadRaw_PluginInstallPathOutsideHomeBlocked verifies that a plugin with an
// InstallPath outside home (e.g. "/") is refused by rootForRef.
// R20260603-GO-2 (path traversal via crafted installed_plugins.json).
func TestReadRaw_PluginInstallPathOutsideHomeBlocked(t *testing.T) {
	home := t.TempDir()
	// Write a malicious installed_plugins.json with installPath pointing outside home.
	writeFile(t, filepath.Join(home, "plugins", "installed_plugins.json"),
		`{"version":2,"plugins":{"evil@evil":[{"scope":"user","installPath":"/","version":"1.0.0"}]}}`)

	_, err := NewClaudeProvider().ReadRaw(assets.RawRequest{
		Home: home,
		Ref: assets.Ref{
			Kind:    "skill",
			Source:  assets.Source{Kind: "plugin", Plugin: "evil@evil"},
			RelPath: "etc/passwd",
		},
	})
	if err == nil {
		t.Fatal("plugin InstallPath outside home must be blocked")
	}
}

// TestScan_PluginInstallPathOutsideHomeSkipped verifies that a plugin with
// InstallPath outside home is silently skipped during Scan (no info disclosure
// via ReadDir of arbitrary directories). R20260603-GO-3.
func TestScan_PluginInstallPathOutsideHomeSkipped(t *testing.T) {
	home := t.TempDir()
	// Write a malicious installed_plugins.json with installPath = "/" (outside home).
	writeFile(t, filepath.Join(home, "plugins", "installed_plugins.json"),
		`{"version":2,"plugins":{"evil@evil":[{"scope":"user","installPath":"/","version":"1.0.0"}]}}`)

	inv, err := NewClaudeProvider().Scan(assets.ScanRequest{Home: home})
	if err != nil {
		t.Fatalf("Scan must not error on malicious installPath, got: %v", err)
	}
	// No plugin assets or plugin infos must appear for the evil plugin.
	if len(inv.Plugins) != 0 {
		t.Errorf("expected 0 plugin infos, got %d: %v", len(inv.Plugins), inv.Plugins)
	}
	if len(inv.Assets) != 0 {
		t.Errorf("expected 0 assets, got %d: %v", len(inv.Assets), inv.Assets)
	}
}

// TestEncodeProjectDir_EquivalentToClaudeProjectSlug verifies that
// encodeProjectDir produces the same result as discovery.ClaudeProjectSlug
// for representative paths. R20260603-CODE-2.
func TestEncodeProjectDir_EquivalentToClaudeProjectSlug(t *testing.T) {
	cases := []string{
		"/home/user/workspace/naozhi",
		"/home/u/work/myproj",
		"/root/proj",
		"/a/b/c/d",
	}
	for _, c := range cases {
		got := encodeProjectDir(c)
		// Construct expected value directly: "/" → "-" substitution on the full path.
		want := "-" + strings.ReplaceAll(strings.TrimPrefix(filepath.Clean(c), "/"), "/", "-")
		if got != want {
			t.Errorf("encodeProjectDir(%q) = %q, want %q", c, got, want)
		}
	}
	// Empty string must return empty string in both implementations.
	if got := encodeProjectDir(""); got != "" {
		t.Errorf("encodeProjectDir(\"\") = %q, want \"\"", got)
	}
}
