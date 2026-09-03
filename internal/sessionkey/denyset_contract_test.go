package sessionkey_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// denySetLiteral matches the invisible-class codepoints of the session-key
// deny-set spelled as a Go hex constant (0x200B) or a string escape (\u200B),
// case-insensitively. C0 / C1 literals (0x20, 0x7F, 0x9F) are deliberately not
// matched: those constants are generic ASCII/Latin-1 boundaries that appear in
// byte-level gates, JSON escaping and display sanitizers all over the repo
// with legitimately different policies, so matching them would be all noise;
// the four hand copies #2301 targets are identified by the invisible block.
var denySetLiteral = regexp.MustCompile(`(?i)0x(200[b-f]|202[a-e]|2028|2029|feff)\b|(?i)\\u(200[b-f]|202[a-e]|2028|2029|feff)`)

// denySetLiteralAllowlist lists the production files outside internal/sessionkey
// that may spell these codepoints. Every entry is a *different* policy, not a
// hand copy of the session-key deny-set — adding a file here needs the same
// justification. Stale entries (file gone, or literal gone) fail the test so
// the list cannot rot.
var denySetLiteralAllowlist = map[string]string{
	"internal/osutil/loginject.go":    "IsLogInjectionRune: covers bidi isolates U+2066..U+2069 and deliberately skips ZWSP / BOM (test-pinned); not reusable for keys",
	"internal/gitinfo/gitinfo.go":     "isUnsafeNameRune: display-name sanitizer with word joiner + bidi isolates, mirrored by a JS twin",
	"internal/cli/process_shim_io.go": "JSON encoder JS-compat escape of U+2028 / U+2029, not a deny-set",
}

// TestDenySetLiteralsLiveOnlyInSessionkey is the R202606f-ARCH-6 (#2301)
// contract: the session-key invisible-character deny-set must have exactly
// one source. Any non-test Go file in the repo that spells one of the
// codepoints as a literal, and is neither in internal/sessionkey nor on the
// documented allowlist, fails — the fix is to call
// sessionkey.IsForbiddenKeyRune / IsInvisibleKeyRune / SanitizeKeyRune.
//
// Only hex constants and \u escapes are matched. Raw (unescaped) invisible
// runes are deliberately NOT scanned: they appear as illustrative payloads in
// comments (e.g. a literal RLO in internal/cron/scheduler_notice.go and
// internal/dispatch/commands.go), which is a mention of the character, not a
// copy of the deny-set.
func TestDenySetLiteralsLiveOnlyInSessionkey(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	seenAllowlisted := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/sessionkey/") {
			return nil // the owner
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !mayContainDenySetLiteral(src) {
			return nil
		}
		hit := denySetLiteral.FindIndex(src)
		if hit == nil {
			return nil
		}
		if _, ok := denySetLiteralAllowlist[rel]; ok {
			seenAllowlisted[rel] = true
			return nil
		}
		line := 1 + bytes.Count(src[:hit[0]], []byte{'\n'})
		t.Errorf("%s:%d spells session-key deny-set codepoint %q — call sessionkey.IsForbiddenKeyRune / IsInvisibleKeyRune / SanitizeKeyRune instead of hand-copying the set (#2301), or document a genuinely different policy in denySetLiteralAllowlist", rel, line, src[hit[0]:hit[1]])
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for rel, why := range denySetLiteralAllowlist {
		if !seenAllowlisted[rel] {
			t.Errorf("allowlist entry %q (%s) is stale: file missing or no longer spells a deny-set codepoint — remove it", rel, why)
		}
	}
}

// mayContainDenySetLiteral is a cheap byte-level prefilter so the regexp only
// runs on files that can possibly match (every match starts with one of these
// prefixes). Keeps the repo-wide walk well under a second.
func mayContainDenySetLiteral(src []byte) bool {
	for _, p := range [...]string{"0x2", "0xF", "0xf", `\u2`, `\uF`, `\uf`} {
		if bytes.Contains(src, []byte(p)) {
			return true
		}
	}
	return false
}

// repoRoot walks up from the test's working directory to the directory that
// holds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test dir")
		}
		dir = parent
	}
}
