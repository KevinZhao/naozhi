package testhelper

// Unguarded shim reads in Playwright waits (#3425). wsm and WS_STATES exist
// in the page only once test/e2e/e2e-shim.js has run, and a waitForFunction
// predicate that throws is not retried, so `page.waitForFunction(() =>
// wsm.state === ...)` fails at once with a ReferenceError when it polls before
// the shim. Waits on the socket go through test/e2e/shim_wait.js (waitForWs,
// or waitForWsWhere for compound predicates), which polls for the shim first.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// unguardedWsmWait matches a waitForFunction whose arrow predicate starts by
// reading wsm or WS_STATES: bare, through window, or through window.nz.test
// (which throws a TypeError while nz.test is unset). Optional chaining and a
// leading `!!window.wsm &&` guard do not match.
var unguardedWsmWait = regexp.MustCompile(`\.waitForFunction\(\s*(?:async\s+)?(?:\([^)]*\)|\w+)\s*=>\s*(?:\{\s*return\s+)?\(?\s*` +
	`(?:/\*\*[^*]*\*/\s*\(window\)\s*\.\s*|window\s*\.\s*)?(?:nz\s*\.\s*test\s*\.\s*)?(?:wsm|WS_STATES)\b`)

// e2eShimWaitFloor is far below the real spec count; a scan that sees fewer
// files has lost the suite.
const e2eShimWaitFloor = 40

// findUnguardedWsmWaits returns file:line for every match under dir.
func findUnguardedWsmWaits(dir string) (files int, hits []string, err error) {
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
		if !strings.HasSuffix(path, ".js") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		rel, _ := filepath.Rel(dir, path)
		for _, loc := range unguardedWsmWait.FindAllIndex(data, -1) {
			line := 1 + strings.Count(string(data[:loc[0]]), "\n")
			hits = append(hits, fmt.Sprintf("%s:%d", rel, line))
		}
		return nil
	})
	return files, hits, err
}

func TestE2EShimWait_NoUnguardedWsmPredicates(t *testing.T) {
	t.Parallel()
	dir, err := filepath.Abs(filepath.Join("..", "..", "test", "e2e"))
	if err != nil {
		t.Fatal(err)
	}
	files, hits, err := findUnguardedWsmWaits(dir)
	if err != nil {
		t.Fatal(err)
	}
	if files < e2eShimWaitFloor {
		t.Fatalf("scanned %d e2e files, below the floor of %d: the scan has lost the suite", files, e2eShimWaitFloor)
	}
	for _, h := range hits {
		t.Errorf("%s: waitForFunction reads wsm before the e2e shim is known to be installed; "+
			"use waitForWs(page[, state]) or waitForWsWhere(page, fn) from test/e2e/shim_wait.js", h)
	}
}

func TestFindUnguardedWsmWaits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := map[string]string{
		"bad.test.js": "await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);\n" +
			"await page.waitForFunction((s) => wsm.state === s, wsState);\n" +
			"await page.waitForFunction(() => window.nz.test.wsm.state === 'connected');\n" +
			"await page.waitForFunction(() => /** @type {any} */ (window).nz.test.wsm.state === 1);\n" +
			"const d = await (await page.waitForFunction(() => wsm.state === WS_STATES.DISCONNECTED &&\n" +
			"  { poll: x }));\n" +
			"await page.waitForFunction(\n  () => WS_STATES.OFF !== wsm.state\n);\n" +
			"await page.waitForFunction(() => { return wsm.backoff > 1; });\n",
		"good.test.js": "await waitForWs(page);\n" +
			"await waitForWsWhere(page, () => wsm.state === WS_STATES.DISCONNECTED && wsm.backoff >= 4000);\n" +
			"await page.waitForFunction(() => window.nz?.test?.wsm?.state === 'connected');\n" +
			"await page.waitForFunction(() => !!window.wsm && window.wsm.state === window.WS_STATES.CONNECTED);\n" +
			"await page.waitForFunction(() => historyTag !== '');\n" +
			"expect(await page.evaluate(() => wsm.state)).toBe('connected');\n",
		"node_modules/x/index.js": "page.waitForFunction(() => wsm.state);\n",
		"notes.md":                "page.waitForFunction(() => wsm.state);\n",
	}
	for p, s := range src {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, hits, err := findUnguardedWsmWaits(dir)
	if err != nil {
		t.Fatal(err)
	}
	if files != 2 {
		t.Errorf("files=%d, want 2 (node_modules and non-.js files excluded)", files)
	}
	want := []string{"bad.test.js:1", "bad.test.js:2", "bad.test.js:3", "bad.test.js:4", "bad.test.js:5", "bad.test.js:7", "bad.test.js:10"}
	if strings.Join(hits, " ") != strings.Join(want, " ") {
		t.Errorf("hits = %v, want %v", hits, want)
	}
}
