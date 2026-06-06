package ccassets

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/assets"
)

// writeSkill creates <root>/<name>/SKILL.md with the given frontmatter body.
func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

const skillFM = `---
name: %s
description: %s
---

# %s

body text
`

// TestScan_UserAndProjectSkills is the P0 vertical slice: only user-level and
// project-level skills, no plugins, no cache.
func TestScan_UserAndProjectSkills(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()

	userSkills := filepath.Join(home, "skills")
	projSkills := filepath.Join(repo, ".claude", "skills")
	writeSkill(t, userSkills, "learned",
		"---\nname: learned\ndescription: auto-captured patterns\n---\nbody")
	writeSkill(t, projSkills, "dev-workflow",
		"---\nname: dev-workflow\ndescription: design then review\n---\nbody")
	writeSkill(t, projSkills, "triage-findings",
		"---\nname: triage-findings\ndescription: classify findings\n---\nbody")

	p := NewClaudeProvider()
	inv, err := p.Scan(assets.ScanRequest{Home: home, RepoRoot: repo})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if got := len(inv.Assets); got != 3 {
		t.Fatalf("want 3 assets, got %d: %+v", got, inv.Assets)
	}
	if inv.Totals["skill"] != 3 {
		t.Errorf("Totals[skill] = %d, want 3", inv.Totals["skill"])
	}

	bySrc := map[string]int{}
	var devWorkflow *assets.Asset
	for i := range inv.Assets {
		a := &inv.Assets[i]
		bySrc[a.Source.Kind]++
		if a.Kind != "skill" {
			t.Errorf("asset %q has kind %q, want skill", a.Name, a.Kind)
		}
		if a.Name == "dev-workflow" {
			devWorkflow = a
		}
	}
	if bySrc["user"] != 1 || bySrc["project"] != 2 {
		t.Errorf("source split = %v, want user:1 project:2", bySrc)
	}
	if devWorkflow == nil {
		t.Fatal("dev-workflow not found")
	}
	if devWorkflow.Description != "design then review" {
		t.Errorf("dev-workflow desc = %q", devWorkflow.Description)
	}
	if devWorkflow.Source.Kind != "project" {
		t.Errorf("dev-workflow source = %q, want project", devWorkflow.Source.Kind)
	}
}

// TestScan_EmptyRepoRootSkipsProject confirms RepoRoot=="" drops project-level.
func TestScan_EmptyRepoRootSkipsProject(t *testing.T) {
	home := t.TempDir()
	writeSkill(t, filepath.Join(home, "skills"), "learned",
		"---\nname: learned\ndescription: x\n---\nbody")

	p := NewClaudeProvider()
	inv, err := p.Scan(assets.ScanRequest{Home: home, RepoRoot: ""})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(inv.Assets) != 1 || inv.Assets[0].Source.Kind != "user" {
		t.Fatalf("want 1 user asset, got %+v", inv.Assets)
	}
}

// TestScan_MalformedFrontmatterDegrades: a SKILL.md without frontmatter must
// not abort the scan; name falls back to the directory name (§1.2-6).
func TestScan_MalformedFrontmatterDegrades(t *testing.T) {
	home := t.TempDir()
	skills := filepath.Join(home, "skills")
	writeSkill(t, skills, "good", "---\nname: good\ndescription: ok\n---\nbody")
	writeSkill(t, skills, "broken", "# just markdown, no frontmatter\n")

	p := NewClaudeProvider()
	inv, err := p.Scan(assets.ScanRequest{Home: home})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(inv.Assets) != 2 {
		t.Fatalf("want 2 assets (degraded included), got %d", len(inv.Assets))
	}
	var broken *assets.Asset
	for i := range inv.Assets {
		if inv.Assets[i].Name == "broken" {
			broken = &inv.Assets[i]
		}
	}
	if broken == nil {
		t.Fatal("broken skill dropped; degrade path failed")
	}
	if broken.Description != "" {
		t.Errorf("broken desc = %q, want empty", broken.Description)
	}
}

// TestScan_SubdirWithoutSkillMdSkipped pins R220123-PERF-4: scanSkillDir now
// uses readFrontmatter's os.Open as the existence probe (no per-subdir
// os.Stat). A subdir lacking SKILL.md, and a subdir where "SKILL.md" is
// itself a directory, must both be skipped — not surfaced as assets and not
// abort the scan.
func TestScan_SubdirWithoutSkillMdSkipped(t *testing.T) {
	home := t.TempDir()
	skills := filepath.Join(home, "skills")
	// Valid skill — must be kept.
	writeSkill(t, skills, "real", "---\nname: real\ndescription: ok\n---\nbody")
	// Subdir with no SKILL.md at all.
	if err := os.MkdirAll(filepath.Join(skills, "notaskill"), 0o755); err != nil {
		t.Fatalf("mkdir notaskill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skills, "notaskill", "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	// Subdir where SKILL.md is a directory (open succeeds, read fails EISDIR).
	if err := os.MkdirAll(filepath.Join(skills, "weird", "SKILL.md"), 0o755); err != nil {
		t.Fatalf("mkdir weird/SKILL.md: %v", err)
	}

	p := NewClaudeProvider()
	inv, err := p.Scan(assets.ScanRequest{Home: home})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(inv.Assets) != 1 || inv.Assets[0].Name != "real" {
		t.Fatalf("want only the 'real' skill, got %+v", inv.Assets)
	}
}

// TestScan_EmptySkillMdDegrades pins that a zero-byte SKILL.md (open ok, EOF
// on first read) is treated as present-but-frontmatter-less and degrades to
// name=<dir>, not skipped as a non-existent file.
func TestScan_EmptySkillMdDegrades(t *testing.T) {
	home := t.TempDir()
	skills := filepath.Join(home, "skills")
	writeSkill(t, skills, "empty", "")

	p := NewClaudeProvider()
	inv, err := p.Scan(assets.ScanRequest{Home: home})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(inv.Assets) != 1 || inv.Assets[0].Name != "empty" {
		t.Fatalf("want degraded 'empty' skill, got %+v", inv.Assets)
	}
}

// TestScan_NoSkillsDir: missing dirs are not an error, just empty.
func TestScan_NoSkillsDir(t *testing.T) {
	p := NewClaudeProvider()
	inv, err := p.Scan(assets.ScanRequest{Home: t.TempDir(), RepoRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(inv.Assets) != 0 {
		t.Fatalf("want 0 assets, got %d", len(inv.Assets))
	}
	if inv.Totals == nil {
		t.Error("Totals should be non-nil even when empty")
	}
}

// TestReadRaw_UserSkill reads a skill's raw bytes via Ref.
func TestReadRaw_UserSkill(t *testing.T) {
	home := t.TempDir()
	writeSkill(t, filepath.Join(home, "skills"), "learned",
		"---\nname: learned\ndescription: x\n---\nhello body")

	p := NewClaudeProvider()
	raw, err := p.ReadRaw(assets.RawRequest{
		Home: home,
		Ref: assets.Ref{
			Kind:    "skill",
			Source:  assets.Source{Kind: "user"},
			RelPath: "skills/learned/SKILL.md",
		},
	})
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if want := "hello body"; !contains(string(raw), want) {
		t.Errorf("raw missing %q: %s", want, raw)
	}
}

// TestReadRaw_Traversal rejects ../ escapes.
func TestReadRaw_Traversal(t *testing.T) {
	home := t.TempDir()
	// plant a secret outside the skills root
	if err := os.WriteFile(filepath.Join(home, "secret.txt"), []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(home, "skills"), "learned", "---\nname: learned\n---\nbody")

	p := NewClaudeProvider()
	_, err := p.ReadRaw(assets.RawRequest{
		Home: home,
		Ref: assets.Ref{
			Kind:    "skill",
			Source:  assets.Source{Kind: "user"},
			RelPath: "skills/../../secret.txt",
		},
	})
	if err == nil {
		t.Fatal("traversal not rejected")
	}
}

// TestScan_ScanMarkdownDirErrorLogged verifies that a non-ErrNotExist error
// from scanMarkdownDir (e.g. agents dir is a file, not a dir) is tolerated:
// Scan continues without returning an error and the warn path is exercised.
// R20260603000023-CR-12.
func TestScan_ScanMarkdownDirErrorLogged(t *testing.T) {
	home := t.TempDir()
	// Plant a regular *file* where the agents directory would be read.
	// os.ReadDir on a regular file returns a syscall error (not ErrNotExist),
	// which triggers the slog.Warn branch added by CR-12.
	writeFile(t, filepath.Join(home, "agents"), "not-a-directory")

	p := NewClaudeProvider()
	// Scan must not propagate the ReadDir error — it only warns.
	inv, err := p.Scan(assets.ScanRequest{Home: home})
	if err != nil {
		t.Fatalf("Scan must not return error on scanMarkdownDir failure, got: %v", err)
	}
	// No agents expected (the scan was skipped due to error).
	if inv.Totals["agent"] != 0 {
		t.Errorf("Totals[agent] = %d, want 0", inv.Totals["agent"])
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
