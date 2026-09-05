package testhelper

// Bare-sleep ratchet (#2534): test code that waits for an asynchronous effect
// with a fixed sleep is the repo's dominant flakiness source (70 flaky-fix
// commits since April). New waits must poll (testhelper.Eventually) or join a
// channel; a genuinely time-based sleep gets an explicit `// sleep-ok:
// <reason>` on the same line. The counts below may only go down.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// bareSleepBaseline is the number of un-annotated `time.Sleep(` occurrences
// in *_test.go files (2026-09-05, #2534; occurrences, not lines — one line
// carries two, hence 215 vs the issue's line-count 213). Lower it when you remove sleeps;
// raising it is not an option — poll with Eventually or annotate the line
// with `// sleep-ok: <reason>` if the sleep is genuinely about elapsed time
// (e.g. producing a measurable duration, not awaiting an effect).
const bareSleepBaseline = 215

// exemptSleepBaseline counts the `// sleep-ok:` annotated sleeps; also
// ratcheted so exemptions cannot become the new default.
const exemptSleepBaseline = 0

// sleepToken is assembled at runtime so this file does not count itself.
var sleepToken = "time." + "Sleep("

func TestBareSleepRatchet(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	bareByFile := map[string]int{}
	bare, exempt := 0, 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, line := range strings.Split(string(data), "\n") {
			n := strings.Count(line, sleepToken)
			if n == 0 {
				continue
			}
			if strings.Contains(line, "sleep-ok:") {
				exempt += n
				continue
			}
			bare += n
			bareByFile[rel] += n
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if bare > bareSleepBaseline {
		type fc struct {
			file string
			n    int
		}
		var top []fc
		for f, n := range bareByFile {
			top = append(top, fc{f, n})
		}
		sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n })
		if len(top) > 10 {
			top = top[:10]
		}
		var b strings.Builder
		for _, e := range top {
			fmt.Fprintf(&b, "  %3d  %s\n", e.n, e.file)
		}
		t.Fatalf("bare %s count in test files grew: %d > baseline %d.\n"+
			"Waiting for an async effect? Poll with testhelper.Eventually or join a channel.\n"+
			"Genuinely time-based? Annotate the line with `// sleep-ok: <reason>`.\n"+
			"Top offenders:\n%s", sleepToken, bare, bareSleepBaseline, b.String())
	}
	if bare < bareSleepBaseline {
		t.Logf("bare sleep count dropped to %d (baseline %d) — lower bareSleepBaseline in this file", bare, bareSleepBaseline)
	}
	if exempt > exemptSleepBaseline {
		t.Fatalf("sleep-ok exemptions grew: %d > baseline %d — exemptions are for genuinely "+
			"time-based sleeps only; raise the baseline here in the same PR with justification", exempt, exemptSleepBaseline)
	}
}
