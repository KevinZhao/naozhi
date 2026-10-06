package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The comment budget of the repository. Each counts comments breaking one of
// CLAUDE.md's Code Comments rules and may only go down: a count above its
// baseline fails, and so does one below it, so the change that fixes a
// comment lowers the baseline. Raising one needs an approved ledger entry
// (scripts/ratchet-raises.jsonl).
const (
	inFuncBlocksOver5Baseline = 30
	docOver10Baseline         = 35
	packageDocOver60Baseline  = 1
	reviewAnchorsBaseline     = 0
	historyPhrasesBaseline    = 40
	duplicateCommentsBaseline = 0
	misplacedDocsBaseline     = 0
)

// budgetFloor: a repository this size has far more issue references than
// this; a walk that finds fewer has gone blind.
const budgetFloor = 500

func TestBudget(t *testing.T) {
	t.Parallel()
	c, err := count(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range budgetProblems(c) {
		t.Error(p)
	}
}

// budgetProblems reports a blind walk and every counter out of step with its
// baseline, in either direction.
func budgetProblems(c Counts) []string {
	if c.IssueRefs < budgetFloor {
		return []string{fmt.Sprintf("found %d issue references, below the floor of %d: the walk has gone blind", c.IssueRefs, budgetFloor)}
	}
	var out []string
	for _, r := range []struct {
		name          string
		got, baseline int
	}{
		{"InFuncBlocksOver5", c.InFuncBlocksOver5, inFuncBlocksOver5Baseline},
		{"DocOver10", c.DocOver10, docOver10Baseline},
		{"PackageDocOver60", c.PackageDocOver60, packageDocOver60Baseline},
		{"ReviewAnchors", c.ReviewAnchors, reviewAnchorsBaseline},
		{"HistoryPhrases", c.HistoryPhrases, historyPhrasesBaseline},
		{"DuplicateComments", c.DuplicateComments, duplicateCommentsBaseline},
		{"MisplacedDocs", c.MisplacedDocs, misplacedDocsBaseline},
	} {
		switch {
		case r.got > r.baseline:
			out = append(out, fmt.Sprintf("%s grew: %d > baseline %d. See CLAUDE.md § Code Comments; `go run ./tools/comment-budget -list %s` lists them:\n  %s",
				r.name, r.got, r.baseline, r.name, strings.Join(c.Offenders[r.name], "\n  ")))
		case r.got < r.baseline:
			out = append(out, fmt.Sprintf("%s dropped to %d (baseline %d): lower the baseline to %d", r.name, r.got, r.baseline, r.got))
		}
	}
	return out
}

func TestBudgetProblems_BothDirections(t *testing.T) {
	t.Parallel()
	at := Counts{InFuncBlocksOver5: inFuncBlocksOver5Baseline, DocOver10: docOver10Baseline,
		PackageDocOver60: packageDocOver60Baseline, ReviewAnchors: reviewAnchorsBaseline,
		HistoryPhrases: historyPhrasesBaseline, DuplicateComments: duplicateCommentsBaseline,
		MisplacedDocs: misplacedDocsBaseline, IssueRefs: budgetFloor}
	if got := budgetProblems(at); len(got) != 0 {
		t.Fatalf("at baseline: %q", got)
	}
	grew := at
	grew.HistoryPhrases++
	if got := budgetProblems(grew); len(got) != 1 || !strings.Contains(got[0], "HistoryPhrases grew") {
		t.Errorf("growth: %q", got)
	}
	dropped := at
	dropped.DocOver10--
	if got := budgetProblems(dropped); len(got) != 1 || !strings.Contains(got[0], "lower the baseline") {
		t.Errorf("drop: %q", got)
	}
	blind := at
	blind.IssueRefs = 0
	if got := budgetProblems(blind); len(got) != 1 || !strings.Contains(got[0], "gone blind") {
		t.Errorf("blind: %q", got)
	}
}

func TestCountFile(t *testing.T) {
	t.Parallel()
	lines := func(n int, prefix string) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(prefix + "// line\n")
		}
		return b.String()
	}
	src := "// Package p is a fixture.\npackage p\n\n" +
		lines(11, "") + // an 11-line godoc
		"func f() {\n" +
		lines(6, "\t") + // a 6-line block in a body
		"\tx := 1\n" +
		lines(5, "\t") + // a 5-line block: within the limit
		"\t_ = x\n" +
		"\t// see R244-SEC-P3-1 and #1234, #56\n" +
		"\t// this used to be a map; the flag was removed\n" +
		"\t//go:noinline is a directive, not prose (#999)\n" +
		"}\n\n" +
		"// g's doc: short, and the helper previously reserved stays legal.\n" +
		"func g() {}\n\n" +
		"// h is annotated twice.\n\n// h is annotated twice.\nfunc h() {}\n\n" + // an exact duplicate: counts once
		"// i has one note.\n\n// i has a different note.\nfunc i() {}\n\n" + // different text: not a duplicate
		"// j is noted once.\nvar sep = 1\n\n// j is noted once.\nfunc j() {}\n\n" + // same text but split by code: not adjacent
		"var k1 = 1 // trailing note\nvar k2 = 2\nvar k3 = 3 // trailing note\n\n" + // two lines apart, but code between: not a duplicate
		"//go:noinline\n\n//go:noinline\nfunc l() {}\n\n" + // directive-only groups: exempt
		"// m is spaced out.\n\n\n// m is spaced out.\nfunc m() {}\n" // two blank lines apart: not adjacent
	var c Counts
	c.Offenders = map[string][]string{}
	if _, err := countFile(&c, token.NewFileSet(), "p.go", []byte(src)); err != nil {
		t.Fatal(err)
	}
	want := Counts{InFuncBlocksOver5: 1, DocOver10: 1, ReviewAnchors: 1, HistoryPhrases: 2, DuplicateComments: 1, IssueRefs: 1}
	if c.InFuncBlocksOver5 != want.InFuncBlocksOver5 || c.DocOver10 != want.DocOver10 ||
		c.ReviewAnchors != want.ReviewAnchors || c.HistoryPhrases != want.HistoryPhrases ||
		c.DuplicateComments != want.DuplicateComments || c.IssueRefs != want.IssueRefs {
		t.Errorf("counts = %+v, want %+v", c, want)
	}
	gen := "// Code generated by x; DO NOT EDIT.\n\npackage p\n\n" + lines(20, "") + "func f() {}\n"
	var g Counts
	g.Offenders = map[string][]string{}
	if f, err := countFile(&g, token.NewFileSet(), "gen.go", []byte(gen)); err != nil || f != nil {
		t.Fatal(f, err)
	}
	if g.DocOver10 != 0 {
		t.Errorf("a generated file was counted: %+v", g)
	}
}

// TestCountMisplacedDocs: a doc counts when it opens with another name of its
// package, from any of its files, or with an identifier-shaped name declared
// nowhere, and when it trails a file's last declaration.
func TestCountMisplacedDocs(t *testing.T) {
	t.Parallel()
	a := "package p\n\n" +
		"// T is documented correctly.\ntype T struct{}\n\n" +
		"// Run starts T.\nfunc (T) Run() {}\n\n" +
		"// Stop names a method of another file.\nfunc (T) Halt() {}\n\n" + // hit
		"// helper names a function of another file.\nvar wired = 1\n\n" + // hit
		"// getFoo was renamed.\nfunc Foo() {}\n\n" + // hit: identifier-shaped, declared nowhere
		"// Default prose opens this doc.\nconst limit = 1\n\n" +
		"// CronRun{A,B}Total count runs by outcome.\nvar CronRunATotal = 1\n\n" + // a family doc
		"// T.Run is fine dotted.\nfunc (T) Spin() {}\n\n" + // hit: Run is not Spin
		"var (\n\t// guarded is guarded by guardedMu.\n\tguardedMu int\n\tguarded   int\n)\n\n" + // names a sibling spec
		"// T satisfies fmt.Stringer.\nvar _ = T{}\n\n" + // a blank declaration
		"// old_name was renamed.\nfunc New() {}\n\n" + // hit: snake_case, declared nowhere
		"// naïve prose opens this doc.\nconst mode = 1\n\n" + // a non-ASCII word, not an identifier
		"var last = 1 // a trailing note\n\n" +
		"// orphan documents nothing.\n" // hit
	b := "package p\n\n// Stop halts.\nfunc Stop() {}\n\n// helper helps.\nfunc helper() {}\n\n" +
		"type Server struct{}\n\ntype Config struct{}\n\ntype Persister struct{}\n"
	// Prose opening with a package name: a compound or possessive, or a name
	// the declaration's own signature, value or type uses.
	prose := "package p\n\nimport \"context\"\n\n" +
		"// Server-side check: check validates it.\nfunc check() {}\n\n" +
		"// Config returned by Load is never nil.\nfunc Load() *Config { return nil }\n\n" +
		"// Config: the defaults.\nvar defaults = 1\n\n" +
		"// Server's address is fixed.\nvar addr = 1\n\n" +
		"// Server is the default one.\nvar srv = Server{}\n\n" +
		"// Config is wrapped here.\ntype wrapped struct{ c Config }\n\n" +
		"// Persister writes logs.\nfunc (p *Persister) Flush() {}\n\n" + // hit: the receiver is not exempt
		"// Stop halts the loop.\nfunc Run(ctx context.Context) {}\n\n" + // hit: Stop is not in the signature
		"// Config is read by the closure.\nvar opened = func() { _ = Config{} }\n\n" + // hit: a function body is not exempt
		"// Config bounds the set.\ntype set[K Config] struct{}\n\n" +
		"// Config is embedded.\ntype embeds struct{ Config }\n\n" +
		"// Stop is the field.\ntype hooks struct{ Stop func() }\n\n" + // hit: a field name is not a use
		"// Flush writes the batch.\ntype flusher interface{ Flush() }\n\n" + // hit: a method name is not a use
		"// helper runs here.\nfunc call(helper int) {}\n\n" + // hit: a param name is not a use
		"var (\n\t// Server is good.\n\tc1 = 1\n\td1 Server\n)\n" // hit: a sibling spec's type is not a use
	note := "package p\n\n// note.go is a design note with no declarations.\n"
	var c Counts
	c.Offenders = map[string][]string{}
	fset := token.NewFileSet()
	var files []*ast.File
	for name, src := range map[string]string{"a.go": a, "b.go": b, "note.go": note, "prose.go": prose} {
		f, err := countFile(&c, fset, name, []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	countMisplacedDocs(&c, fset, files)
	got := c.Offenders["MisplacedDocs"]
	slices.Sort(got)
	want := []string{"a.go:12", "a.go:15", "a.go:24", "a.go:36", "a.go:44", "a.go:9", "prose.go:23", "prose.go:26", "prose.go:29", "prose.go:38", "prose.go:41", "prose.go:44", "prose.go:48"}
	if c.MisplacedDocs != len(want) || !slices.Equal(got, want) {
		t.Errorf("MisplacedDocs = %d at %v, want %v", c.MisplacedDocs, got, want)
	}
}

// TestDocSubject: the subject is a token followed by whitespace or the end of
// the text; one followed by other punctuation is a prose compound.
func TestDocSubject(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]string{
		"Server-side check.": "",
		"Config: defaults.":  "",
		"Server's address.":  "",
		"Run(ctx) starts.":   "",
		"T.Run is fine.":     "Run",
		"Foo.":               "Foo",
		"Foo":                "Foo",
		"Foo ends here.":     "Foo",
		"  Foo\tis indented": "Foo",
		"naïve prose.":       "naïve",
		"- a list item":      "",
	} {
		doc := &ast.CommentGroup{List: []*ast.Comment{{Text: "// " + text}}}
		if got := docSubject(doc); got != want {
			t.Errorf("docSubject(%q) = %q, want %q", text, got, want)
		}
	}
}

// TestCount_Walk: the walk counts non-test Go files under a root named naozhi
// (CI's checkout, the owner's worktree) and skips the old clone nested in it,
// agent worktrees and other nested checkouts, test files, testdata and
// vendored trees.
func TestCount_Walk(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "naozhi")
	src := "package p\n\n// f was removed once.\nfunc f() {}\n"
	for _, p := range []string{
		"internal/a/a.go",
		"cmd/naozhi/main.go",
		"naozhi/internal/a/a.go",
		"internal/a/a_test.go",
		"internal/a/testdata/x.go",
		"test/e2e/node_modules/x/x.go",
		".claude/worktrees/agent-x/internal/a/a.go",
		"wt/.git",
		"wt/internal/a/a.go",
	} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := count(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.HistoryPhrases != 2 {
		t.Errorf("HistoryPhrases = %d, want 2 (internal/a/a.go and cmd/naozhi/main.go only): %v", c.HistoryPhrases, c.Offenders["HistoryPhrases"])
	}
}
