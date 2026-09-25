package testhelper

// Source-anchor ratchet (#2716, Epic I's follow-through). A test that reads .go
// source — a ReadFile of a literal .go path, or a go/parser ParseFile over a
// real file — asserts
// the SHAPE of the code instead of its behaviour. Some of those are the right
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
// Adding a new source-reading test: it starts life needing an anchor-keep line
// AND a lower... no — it raises both counters, so it fails here. That is the
// point: the burden of proof sits with the new anchor, not with the reviewer
// noticing it.

import (
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
const anchorFileBaseline = 52

// unjustifiedAnchorBaseline counts anchor files lacking an `// anchor-keep:`
// justification line. The triage pass drives this to zero file by file.
const unjustifiedAnchorBaseline = 0

// Patterns assembled at runtime so this file does not count itself (the same
// trick sleep_ratchet_test.go uses for its token).
var (
	readFileGo  = regexp.MustCompile(`os\.Read` + `File\("[\w./-]*\.go"\)`)
	parserOnSrc = regexp.MustCompile(`parser\.Parse` + `File\(`)
	goQuote     = ".go" + `"`
)

func TestSourceAnchorRatchet(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var anchors, unjustified []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".git" || name == "naozhi" {
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
		src := string(data)
		// parser.ParseFile counts only when it targets a .go path (a test
		// parsing a fixture string passes src != nil and does not read source).
		isAnchor := readFileGo.MatchString(src) ||
			(parserOnSrc.MatchString(src) && strings.Contains(src, goQuote))
		if !isAnchor {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		anchors = append(anchors, rel)
		if !strings.Contains(src, "// anchor-keep:") {
			unjustified = append(unjustified, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(anchors)

	if len(anchors) > anchorFileBaseline {
		t.Errorf("source-reading test files grew: %d > baseline %d.\n"+
			"A test that reads .go source pins shape, not behaviour — write a behavioural test instead,\n"+
			"or justify the anchor with an `// anchor-keep: <reason>` line and argue the bump in review.\nFiles:\n  %s",
			len(anchors), anchorFileBaseline, strings.Join(anchors, "\n  "))
	}
	if len(anchors) < anchorFileBaseline {
		t.Logf("source-reading test files dropped to %d (baseline %d) — lower anchorFileBaseline", len(anchors), anchorFileBaseline)
	}
	if len(unjustified) > unjustifiedAnchorBaseline {
		t.Errorf("anchor files without an `// anchor-keep:` justification grew: %d > baseline %d.\nUnjustified:\n  %s",
			len(unjustified), unjustifiedAnchorBaseline, strings.Join(unjustified, "\n  "))
	}
	if len(unjustified) < unjustifiedAnchorBaseline {
		t.Logf("unjustified anchors dropped to %d (baseline %d) — lower unjustifiedAnchorBaseline", len(unjustified), unjustifiedAnchorBaseline)
	}
}
