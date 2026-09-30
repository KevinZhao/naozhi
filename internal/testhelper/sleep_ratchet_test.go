package testhelper

// Bare-sleep ratchet (#2534): test code that waits for an asynchronous effect
// with a fixed sleep is the repo's dominant flakiness source (70 flaky-fix
// commits since April). New waits must poll (testhelper.Eventually) or join a
// channel; a genuinely time-based sleep gets an explicit `// sleep-ok:
// <reason>` on the same line. The counts below may only go down.

// anchor-keep: the ratchet counts bare sleeps by reading every test file; that is its job.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// bareSleepBaseline is the number of un-annotated `time.Sleep(` occurrences in
// *_test.go files — occurrences, not lines, since one line can carry two. Lower
// it when you remove sleeps; raising it is not an option — poll with Eventually
// or annotate the line with `// sleep-ok: <reason>` if the sleep is genuinely
// about elapsed time (producing a measurable duration, not awaiting an effect).
const bareSleepBaseline = 136

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
	bare, exempt, bareByFile, err := countBareSleeps(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range sleepRatchetProblems(bare, exempt, bareByFile) {
		t.Error(p)
	}
}

// countBareSleeps counts sleeps in every test file under root, skipping the
// old clone nested at <root>/naozhi and vendored trees.
func countBareSleeps(root string) (bare, exempt int, bareByFile map[string]int, err error) {
	bareByFile = map[string]int{}
	nested := filepath.Join(root, "naozhi") // an old clone kept inside the main worktree
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path == nested || name == "node_modules" || name == ".git" {
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
	return bare, exempt, bareByFile, err
}

func TestCountBareSleeps_SkipsTheNestedClone(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "naozhi")
	line := sleepToken + "time.Millisecond)\n"
	for p, src := range map[string]string{
		"internal/a/a_test.go":            line + line,
		"cmd/naozhi/b_test.go":            line + sleepToken + "1) // sleep-ok: measures a duration\n",
		"naozhi/internal/a/a_test.go":     line,
		"test/e2e/node_modules/x_test.go": line,
		"internal/a/a.go":                 line,
	} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bare, exempt, _, err := countBareSleeps(root)
	if err != nil {
		t.Fatal(err)
	}
	if bare != 3 || exempt != 1 {
		t.Errorf("bare=%d exempt=%d, want 3/1 (nested clone, node_modules and non-test files skipped; cmd/naozhi counted)", bare, exempt)
	}
}

// sleepRatchetProblems reports how the counts are out of step with the
// baselines, in either direction: a drop fails too, so the PR that removes a
// sleep lowers the baseline and the slack cannot be refilled later.
func sleepRatchetProblems(bare, exempt int, bareByFile map[string]int) []string {
	var out []string
	switch {
	case bare > bareSleepBaseline:
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
		out = append(out, fmt.Sprintf("bare %s count in test files grew: %d > baseline %d.\n"+
			"Waiting for an async effect? Poll with testhelper.Eventually or join a channel.\n"+
			"Genuinely time-based? Annotate the line with `// sleep-ok: <reason>`.\n"+
			"Top offenders:\n%s", sleepToken, bare, bareSleepBaseline, b.String()))
	case bare < bareSleepBaseline:
		out = append(out, fmt.Sprintf("bare sleep count dropped to %d (baseline %d): lower bareSleepBaseline to %d", bare, bareSleepBaseline, bare))
	}
	switch {
	case exempt > exemptSleepBaseline:
		out = append(out, fmt.Sprintf("sleep-ok exemptions grew: %d > baseline %d — exemptions are for genuinely "+
			"time-based sleeps only; raising the baseline needs an approved ledger entry (scripts/ratchet-raises.jsonl)", exempt, exemptSleepBaseline))
	case exempt < exemptSleepBaseline:
		out = append(out, fmt.Sprintf("sleep-ok exemptions dropped to %d (baseline %d): lower exemptSleepBaseline to %d", exempt, exemptSleepBaseline, exempt))
	}
	return out
}

func TestSleepRatchetProblems_BothDirections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		bare, exempt int
		want         string
	}{
		{"at baseline", bareSleepBaseline, exemptSleepBaseline, ""},
		{"bare grew", bareSleepBaseline + 1, exemptSleepBaseline, "grew"},
		{"bare dropped", bareSleepBaseline - 1, exemptSleepBaseline, "lower bareSleepBaseline"},
		{"exempt grew", bareSleepBaseline, exemptSleepBaseline + 1, "exemptions grew"},
	}
	for _, tc := range cases {
		got := sleepRatchetProblems(tc.bare, tc.exempt, map[string]int{"a_test.go": 1})
		switch {
		case tc.want == "" && len(got) != 0:
			t.Errorf("%s: %q, want none", tc.name, got)
		case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
			t.Errorf("%s: %q, want one containing %q", tc.name, got, tc.want)
		}
	}
}
