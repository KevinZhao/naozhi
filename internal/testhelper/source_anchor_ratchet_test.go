package testhelper

// Source-anchor ratchet (#2716, #2898). A test that reads .go source (see
// readsGoSource for what counts) asserts the SHAPE of the code instead of its
// behaviour. Some of those are the right
// tool (an import-ban on a leaf package IS a structural fact), most are a
// mutation-blind substitute for a behavioural test, and the population only
// ever grew until it was counted. Two counters, both may only go down:
//
//   - total source-reading test files;
//   - files WITHOUT an `// anchor-keep:` line — a one-sentence justification at
//     the top of the file for why the anchor is the right tool. Triage
//     (#2716's per-file pass) converts or justifies; this counter is its
//     progress meter.
//
// A new source-reading test raises the total, so it fails here: the burden of
// proof sits with the new anchor, not with the reviewer noticing it. A count
// below its baseline fails too, so the PR that removes an anchor also lowers
// the baseline and nothing can grow back into the slack.

// anchor-keep: the ratchet counts source-reading tests by reading every test file; that is its job.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// anchorFileBaseline is the number of *_test.go files that read Go source.
// Lower it when triage converts or deletes one; raising it is not an option —
// write a behavioural test, or when the anchor genuinely is the right tool
// (import bans, lock-shape pins with documented reasons), add it WITH an
// anchor-keep line and argue the baseline bump in review.
const anchorFileBaseline = 95

// unjustifiedAnchorBaseline counts anchor files lacking an `// anchor-keep:`
// justification line. The triage pass drives this to zero file by file.
const unjustifiedAnchorBaseline = 42

// anchorKeep matches a justification line: `// anchor-keep: <reason>` at the
// start of a line. A mention of the marker inside other prose does not count.
var anchorKeep = regexp.MustCompile(`(?m)^\s*// anchor-` + `keep: \S`)

// anchorFloor is far below any real count. A walk that finds fewer has gone
// blind — as it did when the checkout's own directory was skipped — and a
// blind ratchet passes everything.
const anchorFloor = 40

// sourceAnchorFiles walks root and returns the test files that read Go source,
// and those among them without an anchor-keep line, as root-relative paths.
func sourceAnchorFiles(root string) (anchors, unjustified []string, err error) {
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if SkipRepoDir(root, path, d) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !readsGoSource(src) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		anchors = append(anchors, rel)
		if !anchorKeep.Match(src) {
			unjustified = append(unjustified, rel)
		}
		return nil
	})
	sort.Strings(anchors)
	sort.Strings(unjustified)
	return anchors, unjustified, err
}

func TestSourceAnchorRatchet(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	anchors, unjustified, err := sourceAnchorFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(anchors) < anchorFloor {
		t.Fatalf("found %d source-reading test files, below the floor of %d: the walk has gone blind", len(anchors), anchorFloor)
	}
	checkAnchorCount(t, "source-reading test files", anchors, anchorFileBaseline, "anchorFileBaseline",
		"A test that reads .go source pins shape, not behaviour — write a behavioural test instead,\n"+
			"or justify the anchor with an `// anchor-keep: <reason>` line and argue the bump in review.")
	checkAnchorCount(t, "anchor files without an `// anchor-keep:` justification", unjustified,
		unjustifiedAnchorBaseline, "unjustifiedAnchorBaseline", "")
}

// checkAnchorCount fails when got differs from baseline in either direction.
func checkAnchorCount(t *testing.T, what string, got []string, baseline int, name, advice string) {
	t.Helper()
	if msg := anchorCountProblem(what, got, baseline, name, advice); msg != "" {
		t.Error(msg)
	}
}

// anchorCountProblem describes how got is out of step with baseline, or
// returns "" when they agree.
func anchorCountProblem(what string, got []string, baseline int, name, advice string) string {
	switch {
	case len(got) > baseline:
		return fmt.Sprintf("%s grew: %d > baseline %d.\n%s\nFiles:\n  %s", what, len(got), baseline, advice, strings.Join(got, "\n  "))
	case len(got) < baseline:
		return fmt.Sprintf("%s dropped to %d (baseline %d): lower %s to %d.", what, len(got), baseline, name, len(got))
	}
	return ""
}
