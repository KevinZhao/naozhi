package testhelper

// Fixed-wait ratchet for the Playwright suite (#2905), the e2e counterpart of
// the bare-sleep ratchet: `page.waitForTimeout(ms)` waits for a duration, not
// for the state the test needs, so it is either too short on a slow runner or
// dead time on a fast one. Wait on a locator, an expect.poll, or an event
// instead; a wait that is genuinely about elapsed time gets `// wait-ok:
// <reason>` on the same line. The counts below may only go down.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// e2eWaitBaseline is the number of un-annotated waitForTimeout calls in the
// Playwright specs and helpers.
const e2eWaitBaseline = 68

// exemptE2EWaitBaseline counts the `// wait-ok:` annotated ones, ratcheted so
// the annotation cannot become the new default.
const exemptE2EWaitBaseline = 0

// e2eWaitFloor is far below the real file count; a scan that sees fewer files
// has lost the suite.
const e2eWaitFloor = 40

// waitToken is assembled at runtime so this file's own text stays out of any
// scan that reads Go files for it.
var waitToken = "waitFor" + "Timeout("

// e2eWaitExcluded are scripts under test/e2e that are not tests: the
// screenshot tool waits for animations to settle before capturing.
var e2eWaitExcluded = map[string]bool{"take-screenshots.js": true}

// countE2EWaits scans dir for .js files and counts fixed waits.
func countE2EWaits(dir string) (files, bare, exempt int, bareByFile map[string]int, err error) {
	bareByFile = map[string]int{}
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".js") || e2eWaitExcluded[d.Name()] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		rel, _ := filepath.Rel(dir, path)
		for _, line := range strings.Split(string(data), "\n") {
			n := strings.Count(line, waitToken)
			if n == 0 {
				continue
			}
			if strings.Contains(line, "// wait-ok:") {
				exempt += n
				continue
			}
			bare += n
			bareByFile[rel] += n
		}
		return nil
	})
	return files, bare, exempt, bareByFile, err
}

func TestE2EWaitRatchet(t *testing.T) {
	t.Parallel()
	dir, err := filepath.Abs(filepath.Join("..", "..", "test", "e2e"))
	if err != nil {
		t.Fatal(err)
	}
	files, bare, exempt, byFile, err := countE2EWaits(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range e2eWaitProblems(files, bare, exempt, byFile) {
		t.Error(p)
	}
}

// e2eWaitProblems reports a scan that saw too few files, and how the counts
// are out of step with the baselines, in either direction.
func e2eWaitProblems(files, bare, exempt int, bareByFile map[string]int) []string {
	if files < e2eWaitFloor {
		return []string{fmt.Sprintf("scanned %d e2e files, below the floor of %d: the scan has lost the suite", files, e2eWaitFloor)}
	}
	var out []string
	switch {
	case bare > e2eWaitBaseline:
		var files []string
		for f := range bareByFile {
			files = append(files, f)
		}
		sort.Slice(files, func(i, j int) bool { return bareByFile[files[i]] > bareByFile[files[j]] })
		var b strings.Builder
		for i, f := range files {
			if i == 10 {
				break
			}
			fmt.Fprintf(&b, "  %3d  %s\n", bareByFile[f], f)
		}
		out = append(out, fmt.Sprintf("%s calls in test/e2e grew: %d > baseline %d.\n"+
			"Waiting for a state? Wait on a locator, expect.poll or an event.\n"+
			"Genuinely about elapsed time? Annotate the line with `// wait-ok: <reason>`.\n"+
			"Top files:\n%s", waitToken, bare, e2eWaitBaseline, b.String()))
	case bare < e2eWaitBaseline:
		out = append(out, fmt.Sprintf("%s calls dropped to %d (baseline %d): lower e2eWaitBaseline to %d", waitToken, bare, e2eWaitBaseline, bare))
	}
	switch {
	case exempt > exemptE2EWaitBaseline:
		out = append(out, fmt.Sprintf("wait-ok exemptions grew: %d > baseline %d; raising it needs an approved ledger entry (scripts/ratchet-raises.jsonl)", exempt, exemptE2EWaitBaseline))
	case exempt < exemptE2EWaitBaseline:
		out = append(out, fmt.Sprintf("wait-ok exemptions dropped to %d (baseline %d): lower exemptE2EWaitBaseline to %d", exempt, exemptE2EWaitBaseline, exempt))
	}
	return out
}

func TestE2EWaitProblems_BothDirections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		bare, exempt int
		want         string
	}{
		{"at baseline", e2eWaitBaseline, exemptE2EWaitBaseline, ""},
		{"grew", e2eWaitBaseline + 1, exemptE2EWaitBaseline, "grew"},
		{"dropped", e2eWaitBaseline - 1, exemptE2EWaitBaseline, "lower e2eWaitBaseline"},
		{"exemptions grew", e2eWaitBaseline, exemptE2EWaitBaseline + 1, "exemptions grew"},
	}
	if got := e2eWaitProblems(e2eWaitFloor-1, e2eWaitBaseline, exemptE2EWaitBaseline, nil); len(got) != 1 || !strings.Contains(got[0], "lost the suite") {
		t.Errorf("a scan below the floor: %q", got)
	}
	for _, tc := range cases {
		got := e2eWaitProblems(e2eWaitFloor, tc.bare, tc.exempt, map[string]int{"a.test.js": 1})
		switch {
		case tc.want == "" && len(got) != 0:
			t.Errorf("%s: %q, want none", tc.name, got)
		case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
			t.Errorf("%s: %q, want one containing %q", tc.name, got, tc.want)
		}
	}
}

func TestCountE2EWaits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := "await page." + waitToken + "100);"
	for p, src := range map[string]string{
		"a.test.js":               w + "\n" + w + " " + w + "\n",
		"helpers/nav.js":          w + "\n",
		"b.test.js":               w + " // wait-ok: measures the debounce window\n",
		"take-screenshots.js":     w + "\n",
		"node_modules/x/index.js": w + "\n",
		"check-ws-contract.mjs":   w + "\n",
		"notes/readme.md":         w + "\n",
		"quiet.test.js":           "await expect(page.locator('x')).toBeVisible();\n",
	} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, bare, exempt, byFile, err := countE2EWaits(dir)
	if err != nil {
		t.Fatal(err)
	}
	if files != 4 || bare != 4 || exempt != 1 {
		t.Errorf("files=%d bare=%d exempt=%d, want 4/4/1 (screenshot tool, node_modules and non-.js files excluded)", files, bare, exempt)
	}
	if byFile["a.test.js"] != 3 || byFile[filepath.Join("helpers", "nav.js")] != 1 {
		t.Errorf("byFile = %v", byFile)
	}
}
