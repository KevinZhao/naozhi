package ccassets

import (
	"errors"
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
