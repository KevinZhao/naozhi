package ccassets

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/assets"
)

// TestResolveUnder_RejectsNULAndAbsolute pins #2250: resolveUnder receives a
// user-controlled rel (Ref.RelPath). It must reject a NUL-bearing or absolute
// rel up front — mirroring the dashboard project files.go guard — so a crafted
// "/etc/passwd" cannot discard the allowed root on join, and a NUL cannot slip
// to filepath.Join. Pre-fix only the ".." substring was checked.
func TestResolveUnder_RejectsNULAndAbsolute(t *testing.T) {
	t.Parallel()

	root := t.TempDir() // a real, symlink-resolvable root so the guard, not EvalSymlinks, is exercised
	tests := []struct {
		name string
		rel  string
	}{
		{"nul byte", "skills/x\x00/SKILL.md"},
		{"absolute unix", "/etc/passwd"},
		{"absolute under root literal", "/" + "SKILL.md"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveUnder(root, tc.rel)
			if err == nil {
				t.Fatalf("resolveUnder(%q, %q) = nil err, want rejection", root, tc.rel)
			}
			if !errors.Is(err, assets.ErrNotFound) {
				t.Errorf("resolveUnder error = %v, want wrapping assets.ErrNotFound (errPathEscape)", err)
			}
		})
	}
}

// The user source's root is all of ~/.claude; ReadRaw must serve only what
// Scan lists there, never the credentials, settings or transcripts beside it.
func TestReadRaw_UserSourceOnlyServesListedShapes(t *testing.T) {
	home := t.TempDir()
	files := map[string]string{
		".credentials.json":        "SECRET",
		"settings.json":            "ENV",
		"projects/-x/s.jsonl":      "TRANSCRIPT",
		"skills/learned/SKILL.md":  "skill",
		"skills/learned/notes.txt": "notes",
		"agents/a.md":              "agent",
		"agents/a.txt":             "agent text",
		"commands/c.md":            "command",
		".mcp.json":                "{}",
	}
	for rel, body := range files {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	allowed := map[string]bool{
		"skills/learned/SKILL.md": true,
		"agents/a.md":             true,
		"commands/c.md":           true,
		".mcp.json":               true,
	}
	for rel, body := range files {
		raw, err := NewClaudeProvider().ReadRaw(assets.RawRequest{
			Home: home,
			Ref:  assets.Ref{Source: assets.Source{Kind: "user"}, RelPath: rel},
		})
		if allowed[rel] {
			if err != nil || string(raw) != body {
				t.Errorf("ReadRaw(%q) = %q, %v; want %q", rel, raw, err, body)
			}
			continue
		}
		if err == nil {
			t.Errorf("ReadRaw(%q) served %q; want refused", rel, raw)
		} else if !errors.Is(err, assets.ErrNotFound) {
			t.Errorf("ReadRaw(%q) error = %v; want ErrNotFound", rel, err)
		}
	}
}
