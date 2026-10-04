package testhelper

// Fixed-wait ratchet for the Playwright suite (#2905), the e2e counterpart of
// the bare-sleep ratchet: `page.waitForTimeout(ms)` and the promise sleep
// `await new Promise(r => setTimeout(r, ms))` (also inside page.evaluate) wait
// for a duration, not for the state the test needs, so they are either too
// short on a slow runner or dead time on a fast one. Wait on a locator, an
// expect.poll, or an event instead; a wait that is genuinely about elapsed time
// gets `// wait-ok: <reason>` on the same line. Each form has its own
// baselines, and the counts below may only go down.

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

// e2eSleepBaseline is the number of un-annotated promise sleeps.
const e2eSleepBaseline = 12

// exemptE2ESleepBaseline counts the `// wait-ok:` annotated promise sleeps.
const exemptE2ESleepBaseline = 2

// e2eWaitFloor is far below the real file count; a scan that sees fewer files
// has lost the suite.
const e2eWaitFloor = 40

// The tokens are assembled at runtime so this file's own text stays out of any
// scan that reads Go files for them.
var (
	waitToken    = "waitFor" + "Timeout("
	promiseToken = "new " + "Promise("
	timerToken   = "set" + "Timeout("
)

// e2eWaitExcluded are scripts under test/e2e that are not tests: the
// screenshot tool waits for animations to settle before capturing.
var e2eWaitExcluded = map[string]bool{"take-screenshots.js": true}

// e2eWaitGate is one form of fixed wait with its own pair of baselines.
type e2eWaitGate struct {
	what                     string // as it appears in failure messages
	baseline, exempt         int
	baselineName, exemptName string
	count                    func(line string) int
}

var e2eWaitGates = []e2eWaitGate{
	{waitToken + " calls", e2eWaitBaseline, exemptE2EWaitBaseline, "e2eWaitBaseline", "exemptE2EWaitBaseline",
		func(line string) int { return strings.Count(line, waitToken) }},
	{"promise sleeps", e2eSleepBaseline, exemptE2ESleepBaseline, "e2eSleepBaseline", "exemptE2ESleepBaseline",
		countPromiseSleeps},
}

// countPromiseSleeps counts the setTimeout calls on a line that builds a
// Promise. Stub callbacks and mock-server reply delays never share a line with
// `new Promise(`; a sleep whose setTimeout sits on a later line than its
// Promise is not seen.
func countPromiseSleeps(line string) int {
	if !strings.Contains(line, promiseToken) {
		return 0
	}
	return strings.Count(line, timerToken)
}

// e2eWaitCount is one gate's tally over the suite.
type e2eWaitCount struct {
	bare, exempt int
	bareByFile   map[string]int
}

// countE2EWaits scans dir for .js files and tallies each gate's fixed waits.
func countE2EWaits(dir string, gates []e2eWaitGate) (files int, counts []e2eWaitCount, err error) {
	counts = make([]e2eWaitCount, len(gates))
	for i := range counts {
		counts[i].bareByFile = map[string]int{}
	}
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
			exempt := strings.Contains(line, "// wait-ok:")
			for i, g := range gates {
				n := g.count(line)
				switch {
				case n == 0:
				case exempt:
					counts[i].exempt += n
				default:
					counts[i].bare += n
					counts[i].bareByFile[rel] += n
				}
			}
		}
		return nil
	})
	return files, counts, err
}

func TestE2EWaitRatchet(t *testing.T) {
	t.Parallel()
	dir, err := filepath.Abs(filepath.Join("..", "..", "test", "e2e"))
	if err != nil {
		t.Fatal(err)
	}
	files, counts, err := countE2EWaits(dir, e2eWaitGates)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range e2eWaitProblems(files, e2eWaitGates, counts) {
		t.Error(p)
	}
}

// e2eWaitProblems reports a scan that saw too few files, and how each gate's
// counts are out of step with its baselines, in either direction.
func e2eWaitProblems(files int, gates []e2eWaitGate, counts []e2eWaitCount) []string {
	if files < e2eWaitFloor {
		return []string{fmt.Sprintf("scanned %d e2e files, below the floor of %d: the scan has lost the suite", files, e2eWaitFloor)}
	}
	var out []string
	for i, g := range gates {
		out = append(out, e2eGateProblems(g, counts[i])...)
	}
	return out
}

func e2eGateProblems(g e2eWaitGate, c e2eWaitCount) []string {
	var out []string
	switch {
	case c.bare > g.baseline:
		var files []string
		for f := range c.bareByFile {
			files = append(files, f)
		}
		sort.Slice(files, func(i, j int) bool { return c.bareByFile[files[i]] > c.bareByFile[files[j]] })
		var b strings.Builder
		for i, f := range files {
			if i == 10 {
				break
			}
			fmt.Fprintf(&b, "  %3d  %s\n", c.bareByFile[f], f)
		}
		out = append(out, fmt.Sprintf("%s in test/e2e grew: %d > baseline %d.\n"+
			"Waiting for a state? Wait on a locator, expect.poll or an event.\n"+
			"Genuinely about elapsed time? Annotate the line with `// wait-ok: <reason>`.\n"+
			"Top files:\n%s", g.what, c.bare, g.baseline, b.String()))
	case c.bare < g.baseline:
		out = append(out, fmt.Sprintf("%s dropped to %d (baseline %d): lower %s to %d", g.what, c.bare, g.baseline, g.baselineName, c.bare))
	}
	switch {
	case c.exempt > g.exempt:
		out = append(out, fmt.Sprintf("wait-ok exemptions on %s grew: %d > baseline %d; raising it needs an approved ledger entry (scripts/ratchet-raises.jsonl)", g.what, c.exempt, g.exempt))
	case c.exempt < g.exempt:
		out = append(out, fmt.Sprintf("wait-ok exemptions on %s dropped to %d (baseline %d): lower %s to %d", g.what, c.exempt, g.exempt, g.exemptName, c.exempt))
	}
	return out
}

func TestE2EWaitProblems_BothDirections(t *testing.T) {
	t.Parallel()
	atBaseline := func() []e2eWaitCount {
		out := make([]e2eWaitCount, len(e2eWaitGates))
		for i, g := range e2eWaitGates {
			out[i] = e2eWaitCount{bare: g.baseline, exempt: g.exempt, bareByFile: map[string]int{"a.test.js": 1}}
		}
		return out
	}
	if got := e2eWaitProblems(e2eWaitFloor-1, e2eWaitGates, atBaseline()); len(got) != 1 || !strings.Contains(got[0], "lost the suite") {
		t.Errorf("a scan below the floor: %q", got)
	}
	if got := e2eWaitProblems(e2eWaitFloor, e2eWaitGates, atBaseline()); len(got) != 0 {
		t.Errorf("at baseline: %q, want none", got)
	}
	for i, g := range e2eWaitGates {
		cases := []struct {
			name         string
			bare, exempt int
			want         string
		}{
			{"grew", g.baseline + 1, g.exempt, g.what + " in test/e2e grew"},
			{"dropped", g.baseline - 1, g.exempt, fmt.Sprintf("lower %s to %d", g.baselineName, g.baseline-1)},
			{"exemptions grew", g.baseline, g.exempt + 1, "exemptions on " + g.what + " grew"},
			{"exemptions dropped", g.baseline, g.exempt - 1, fmt.Sprintf("lower %s to %d", g.exemptName, g.exempt-1)},
		}
		for _, tc := range cases {
			if tc.exempt < 0 {
				continue
			}
			counts := atBaseline()
			counts[i].bare, counts[i].exempt = tc.bare, tc.exempt
			got := e2eWaitProblems(e2eWaitFloor, e2eWaitGates, counts)
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Errorf("%s %s: %q, want one containing %q", g.what, tc.name, got, tc.want)
			}
		}
	}
}

func TestCountE2EWaits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := "await page." + waitToken + "100);"
	sleep := "await " + promiseToken + "r => " + timerToken + "r, 100));"
	for p, src := range map[string]string{
		"a.test.js":               w + "\n" + w + " " + w + "\n",
		"helpers/nav.js":          w + "\n",
		"b.test.js":               w + " // wait-ok: measures the debounce window\n",
		"take-screenshots.js":     w + "\n" + sleep + "\n",
		"node_modules/x/index.js": w + "\n" + sleep + "\n",
		"check-ws-contract.mjs":   w + "\n" + sleep + "\n",
		"notes/readme.md":         w + "\n" + sleep + "\n",
		"quiet.test.js":           "await expect(page.locator('x')).toBeVisible();\n",
		"sleeps.test.js": sleep + "\n" +
			"await " + promiseToken + "(r) => " + timerToken + "r, 100));\n" +
			"await page.evaluate(() => " + promiseToken + "(r) => " + timerToken + "r, 300)));\n" +
			"await " + promiseToken + "r => " + timerToken + "r, DELAY)); // wait-ok: simulated backend latency\n" +
			w + " " + sleep + "\n",
		"timers.test.js": timerToken + "() => { cb(); }, 30);\n" +
			"if (delay > 0) " + timerToken + "reply, delay);\n" +
			"clearTimeout(t);\n" +
			"expect(js).toContain('searchDebounce = " + timerToken + "');\n" +
			"await " + promiseToken + "r => mock.server.close(r));\n",
	} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, counts, err := countE2EWaits(dir, e2eWaitGates)
	if err != nil {
		t.Fatal(err)
	}
	if files != 6 {
		t.Errorf("files=%d, want 6 (screenshot tool, node_modules and non-.js files excluded)", files)
	}
	waits, sleeps := counts[0], counts[1]
	if waits.bare != 5 || waits.exempt != 1 {
		t.Errorf("waits bare=%d exempt=%d, want 5/1", waits.bare, waits.exempt)
	}
	if waits.bareByFile["a.test.js"] != 3 || waits.bareByFile[filepath.Join("helpers", "nav.js")] != 1 || waits.bareByFile["sleeps.test.js"] != 1 {
		t.Errorf("waits byFile = %v", waits.bareByFile)
	}
	if sleeps.bare != 4 || sleeps.exempt != 1 {
		t.Errorf("sleeps bare=%d exempt=%d, want 4/1 (both arrow forms, page.evaluate, and the line shared with a wait)", sleeps.bare, sleeps.exempt)
	}
	if len(sleeps.bareByFile) != 1 || sleeps.bareByFile["sleeps.test.js"] != 4 {
		t.Errorf("sleeps byFile = %v, want only sleeps.test.js: 4 (timer callbacks are not sleeps)", sleeps.bareByFile)
	}
}
