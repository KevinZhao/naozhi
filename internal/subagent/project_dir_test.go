package subagent

import (
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
)

// TestProjectDir_UsesTheVerifiedSlugEncoding guards the bug this function had:
// it hand-rolled the CWD→directory encoding, writing one '-' per RUNE, while the
// Claude CLI substitutes per UTF-16 CODE UNIT. A non-BMP rune (an emoji in the
// workspace path) therefore produced one dash here and two in the directory the
// CLI actually created, so Resolve looked for the transcript somewhere the CLI
// never wrote and subagent linking silently found nothing.
//
// It was the seventh copy of that encoding in the tree. #2643 consolidated six;
// this one hid behind a different name (resolveProjectDir) and a hand-written
// switch instead of a regex, so the grep that found the others missed it.
//
// claudefs.ProjectSlug is the copy verified against CLI 2.1.219, including the
// non-BMP case (claudefs.TestClaudeProjectSlug_NonASCIIUTF16Parity). Pinning
// against it — rather than against a golden string — means a future correction to
// the CLI's real behaviour lands in one place and this test follows.
func TestProjectDir_UsesTheVerifiedSlugEncoding(t *testing.T) {
	root := claudefs.ProjectsRoot(claudefs.DefaultDir())
	if root == "" {
		t.Skip("no resolvable home directory; DefaultDir() is empty")
	}
	for _, cwd := range []string{
		"/home/u/proj",
		"/tmp/中文目录",                 // BMP: one dash per rune, same either way
		"/tmp/emoji-\U0001F389-dir", // non-BMP: TWO dashes — the case that differed
		"/tmp/a.b",
	} {
		want := filepath.Join(root, claudefs.ProjectSlug(cwd))
		if got := ProjectDir(cwd); got != want {
			t.Errorf("ProjectDir(%q)\n got %q\nwant %q", cwd, got, want)
		}
	}
}
