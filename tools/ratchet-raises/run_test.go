package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fakeTree map[string]string

func (f fakeTree) read(path string) (string, error) { return f[path], nil }

func (f fakeTree) baselineGoFiles() ([]string, error) {
	var out []string
	for p := range f {
		if strings.HasSuffix(p, ".go") {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out, nil
}

func TestRun_EveryBaselineSourceIsRead(t *testing.T) {
	t.Parallel()
	base := fakeTree{
		"internal/testhelper/sleep_ratchet_test.go": "package testhelper\nconst bareSleepBaseline = 138\n",
		jsRatchetPath:  `{"a.js":{"lines":10,"maxFnLines":3}}`,
		jsDepsPath:     `{"matrix":{},"tdz":{},"typeofGuards":{},"bridgeRefs":{}}`,
		exemptionsPath: "file_size: []\nhandle_baseline: []\n",
	}
	head := fakeTree{
		"internal/testhelper/sleep_ratchet_test.go": "package testhelper\nconst bareSleepBaseline = 139\n",
		jsRatchetPath:  `{"a.js":{"lines":11,"maxFnLines":3}}`,
		jsDepsPath:     `{"matrix":{"a.js":{"b.js":["x"]}},"tdz":{},"typeofGuards":{},"bridgeRefs":{}}`,
		exemptionsPath: "file_size:\n  - path: s.go\n    current: 600\n    limit: 500\n    until: \"2027-03-31\"\nhandle_baseline: []\n",
	}
	problems, rs, err := run(base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"exemptions:file_size:s.go",
		"go:internal/testhelper#bareSleepBaseline",
		"js-deps:matrix:a.js>b.js:x",
		"js-ratchet:TOTAL.lines",
	}
	if got := gates(rs); !slices.Equal(got, want) {
		t.Fatalf("raises = %v, want %v", got, want)
	}
	if len(problems) != len(want) {
		t.Errorf("problems = %q, want one missing-entry problem per raise", problems)
	}
}

// Creating scripts/js-ratchet.caps.json for the first time must not demand a
// ledger entry for each of its initial exempt/legacy entries: they are
// today's real violations being recorded, not new ones (#3025 S19-0).
func TestRun_CreatingCapsIsNotARaise(t *testing.T) {
	t.Parallel()
	base := fakeTree{}
	head := fakeTree{
		jsCapsPath: `{"maxFnLines":{"default":120,"exempt":["a.js"]},"lines":{"dashboard.js":6511},"sideEffectLegacy":["a.js"],"cycleLegacy":[]}`,
	}
	problems, rs, err := run(base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 0 {
		t.Errorf("raises = %v, want none", rs)
	}
	if len(problems) != 0 {
		t.Errorf("problems = %v, want none", problems)
	}
}

// Once caps.json exists in base, a later addition to one of its lists is an
// ordinary raise again.
func TestRun_WideningCapsAfterItExistsIsARaise(t *testing.T) {
	t.Parallel()
	const existing = `{"maxFnLines":{"default":120,"exempt":["a.js"]},"lines":{"dashboard.js":6511},"sideEffectLegacy":["a.js"],"cycleLegacy":[]}`
	base := fakeTree{jsCapsPath: existing}
	head := fakeTree{
		jsCapsPath: `{"maxFnLines":{"default":120,"exempt":["a.js","b.js"]},"lines":{"dashboard.js":6511},"sideEffectLegacy":["a.js"],"cycleLegacy":[]}`,
	}
	_, rs, err := run(base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"js-caps:exempt:b.js"}
	if got := gates(rs); !slices.Equal(got, want) {
		t.Fatalf("raises = %v, want %v", got, want)
	}
}

// The analysis lists js-ratchet reads from caps.json (S20a): the PR that
// moves one into an existing document records its entries; after that a new
// allowed receiver or shell root, or a dropped or re-pointed late-binding
// table, is a raise.
func TestRun_CapsAnalysisLists(t *testing.T) {
	t.Parallel()
	const pre = `{"maxFnLines":{"default":120,"exempt":[]},"lines":{},"sideEffectLegacy":[],"cycleLegacy":[],"leaves":[]`
	doc := func(rest string) fakeTree { return fakeTree{jsCapsPath: pre + rest + "}"} }
	const now = `,"lateBindingTables":{"hooks":"state.js","nzViews":"nz_util.js"},"injectionAllow":["nz_util.js:registerActions"],"shellRoots":["dashboard.js"]`
	t.Run("adding the lists to an existing document", func(t *testing.T) {
		_, rs, err := run(doc(""), doc(now), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rs) != 0 {
			t.Errorf("raises = %v, want none", rs)
		}
	})
	for _, tc := range []struct {
		name, head string
		want       []string
	}{
		{"an allowed receiver", `,"lateBindingTables":{"hooks":"state.js","nzViews":"nz_util.js"},"injectionAllow":["nz_util.js:registerActions","voice.js:configureVoice"],"shellRoots":["dashboard.js"]`,
			[]string{"js-caps:injectionAllow:voice.js:configureVoice"}},
		{"a shell root", `,"lateBindingTables":{"hooks":"state.js","nzViews":"nz_util.js"},"injectionAllow":["nz_util.js:registerActions"],"shellRoots":["dashboard.js","view.js"]`,
			[]string{"js-caps:shellRoot:view.js"}},
		{"a dropped and a re-pointed table", `,"lateBindingTables":{"hooks":"other.js"},"injectionAllow":["nz_util.js:registerActions"],"shellRoots":["dashboard.js"]`,
			[]string{"js-caps:lateBindingTable:hooks=state.js", "js-caps:lateBindingTable:nzViews=nz_util.js"}},
		{"dropping the allowlist entry and a root is free", `,"lateBindingTables":{"hooks":"state.js","nzViews":"nz_util.js"},"injectionAllow":[],"shellRoots":[]`,
			nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rs, err := run(doc(now), doc(tc.head), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := gates(rs); !slices.Equal(got, tc.want) {
				t.Fatalf("raises = %v, want %v", got, tc.want)
			}
		})
	}
}

// The PR that adds caps.injectionLegacy (S20k) records today's receivers;
// after that a new one is a raise.
func TestRun_CapsInjectionLegacy(t *testing.T) {
	t.Parallel()
	const pre = `{"maxFnLines":{"default":120,"exempt":[]},"lines":{},"sideEffectLegacy":[],"cycleLegacy":[],"leaves":[]`
	doc := func(rest string) fakeTree { return fakeTree{jsCapsPath: pre + rest + "}"} }
	const seeded = `,"injectionLegacy":["tuning.js:configureTuning","discovery.js:configureDiscovery"]`
	_, rs, err := run(doc(""), doc(seeded), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 0 {
		t.Errorf("creating the section: raises = %v, want none", rs)
	}
	_, rs, err = run(doc(seeded), doc(`,"injectionLegacy":["tuning.js:configureTuning","view.js:wireView"]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"js-caps:injectionLegacy:view.js:wireView"}; !slices.Equal(gates(rs), want) {
		t.Errorf("raises = %v, want %v", gates(rs), want)
	}
}

// The first pins.json is recorded, not raised; once base has one, a pin head
// adds is a raise like a changed or deleted one.
func TestRun_GoldenPins_AddedPinRaisesOnlyOnceBaseHasPins(t *testing.T) {
	t.Parallel()
	const one = `{"event_render_known.json":"112233445566"}`
	const two = `{"event_render_known.json":"112233445566","sidebar.json":"aabbccddeeff"}`
	t.Run("creating the document", func(t *testing.T) {
		_, rs, err := run(fakeTree{}, fakeTree{goldenPinsPath: two}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rs) != 0 {
			t.Errorf("raises = %v, want none", rs)
		}
	})
	t.Run("adding a pin to an existing document", func(t *testing.T) {
		_, rs, err := run(fakeTree{goldenPinsPath: one}, fakeTree{goldenPinsPath: two}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"golden:sidebar.json"}
		if got := gates(rs); !slices.Equal(got, want) {
			t.Fatalf("raises = %v, want %v", got, want)
		}
	})
}

// Every way of writing, moving or losing a Go baseline constant either
// shows as a raise or fails the run (#3108): none leaves the ledger with
// nothing to check.
func TestRun_GoBaselineSpellings(t *testing.T) {
	t.Parallel()
	const (
		sleepFile = "internal/testhelper/sleep_ratchet_test.go"
		key       = "go:internal/testhelper#bareSleepBaseline"
	)
	src := func(body string) string { return "package testhelper\n\n" + body + "\n" }
	base := fakeTree{sleepFile: src("const bareSleepBaseline = 135")}
	gone := []raise{{key, 135, -1}}
	for _, tc := range []struct {
		name     string
		base     fakeTree // nil: base above
		head     fakeTree
		want     []raise
		literals int // "must be a plain integer literal" / "declared twice" problems
	}{
		{name: "raised in place", head: fakeTree{sleepFile: src("const bareSleepBaseline = 500")}, want: []raise{{key, 135, 500}}},
		{name: "trailing comment", head: fakeTree{sleepFile: src("const bareSleepBaseline = 500 // raised")}, want: []raise{{key, 135, 500}}},
		{name: "typed", head: fakeTree{sleepFile: src("const bareSleepBaseline int = 500")}, want: []raise{{key, 135, 500}}},
		{name: "hex", head: fakeTree{sleepFile: src("const bareSleepBaseline = 0x1f4")}, want: []raise{{key, 135, 500}}},
		{name: "digit separator", head: fakeTree{sleepFile: src("const bareSleepBaseline = 5_00")}, want: []raise{{key, 135, 500}}},
		{name: "in a const block", head: fakeTree{sleepFile: src("const (\n\tother = 1\n\tbareSleepBaseline = 500\n)")}, want: []raise{{key, 135, 500}}},
		{name: "function-local", head: fakeTree{sleepFile: src("func f() {\n\tconst bareSleepBaseline = 500\n\t_ = bareSleepBaseline\n}")}, want: []raise{{key, 135, 500}}},
		{name: "arithmetic", head: fakeTree{sleepFile: src("const bareSleepBaseline = 400 + 100")}, want: gone, literals: 1},
		{name: "another constant", head: fakeTree{sleepFile: src("const n = 500\nconst bareSleepBaseline = n")}, want: gone, literals: 1},
		{name: "negative", head: fakeTree{sleepFile: src("const bareSleepBaseline = -1")}, want: gone, literals: 1},
		{name: "a string", head: fakeTree{sleepFile: src("const bareSleepBaseline = \"500\"")}, want: gone, literals: 1},
		{name: "implicit repetition", head: fakeTree{sleepFile: src("const (\n\taBaseline = 3\n\tbareSleepBaseline\n)")}, want: gone, literals: 1},
		{name: "iota", head: fakeTree{sleepFile: src("const bareSleepBaseline = iota + 500")}, want: gone, literals: 1},
		{name: "declared twice in the package", head: fakeTree{
			sleepFile:                           src("const bareSleepBaseline = 135"),
			"internal/testhelper/other_test.go": src("func f() {\n\tconst bareSleepBaseline = 900\n\t_ = bareSleepBaseline\n}"),
		}, want: []raise{{key, 135, 900}}, literals: 1},
		{name: "renamed", head: fakeTree{sleepFile: src("const bareSleepBaseline2 = 500")}, want: gone},
		{name: "made a var", head: fakeTree{sleepFile: src("var bareSleepBaseline = 135")}, want: gone},
		{name: "file deleted", head: fakeTree{}, want: gone},
		{name: "file renamed in its package", head: fakeTree{"internal/testhelper/sleep_test.go": src("const bareSleepBaseline = 135")}},
		{name: "file renamed in its package and raised", head: fakeTree{"internal/testhelper/sleep_test.go": src("const bareSleepBaseline = 136")}, want: []raise{{key, 135, 136}}},
		{name: "moved to another package", head: fakeTree{"internal/other/sleep_test.go": src("const bareSleepBaseline = 135")}, want: gone},
		{name: "lowered", head: fakeTree{sleepFile: src("const bareSleepBaseline = 134")}},
		{name: "a new baseline", head: fakeTree{sleepFile: src("const bareSleepBaseline = 135\nconst newBaseline = 7")}},
		{name: "a non-literal only in base", base: fakeTree{sleepFile: src("const bareSleepBaseline = 135\nconst oldBaseline = `x`")},
			head: fakeTree{sleepFile: src("const bareSleepBaseline = 135")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := tc.base
			if b == nil {
				b = base
			}
			problems, rs, err := run(b, tc.head, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(rs, tc.want) {
				t.Errorf("raises = %v, want %v", rs, tc.want)
			}
			literals := 0
			for _, p := range problems {
				if strings.Contains(p, "plain integer literal") || strings.Contains(p, "declared twice") {
					literals++
				}
			}
			if literals != tc.literals || len(problems) != literals+len(tc.want) {
				t.Errorf("problems = %q, want %d about the constant and one per raise", problems, tc.literals)
			}
		})
	}
}

// Keeping a baseline constant while dropping the code that compares against
// it loosens the ratchet as surely as raising it: the last use going away
// is a raise to -1, and deleting the constant with its test is still one.
func TestRun_GoBaselineLosesItsLastUse(t *testing.T) {
	t.Parallel()
	const (
		dir       = "internal/testhelper/"
		constFile = dir + "sleep_ratchet_test.go"
		ref       = "go-ref:internal/testhelper#bareSleepBaseline"
	)
	src := func(body string) string { return "package testhelper\n\n" + body + "\n" }
	const (
		decl = "const bareSleepBaseline = 135"
		test = "func TestX(t *testing.T) {\n\tif n > bareSleepBaseline {\n\t\tt.Fatal()\n\t}\n}"
	)
	used := fakeTree{constFile: src(decl + "\n" + test)}
	unused := fakeTree{constFile: src(decl)}
	lost := []raise{{ref, 1, -1}}
	for _, tc := range []struct {
		name    string
		base    fakeTree // nil: used
		head    fakeTree
		want    []raise
		cleared bool
	}{
		{name: "unchanged", head: used},
		{name: "test deleted", head: unused, want: lost},
		{name: "comparison commented out", head: fakeTree{constFile: src(decl + "\n// " + strings.ReplaceAll(test, "\n", "\n// "))}, want: lost},
		{name: "blank assignment", head: fakeTree{constFile: src(decl + "\nfunc init() { _ = bareSleepBaseline }")}, want: lost},
		{name: "call result discarded", head: fakeTree{constFile: src(decl + "\nfunc TestX(t *testing.T) { _ = below(t, bareSleepBaseline) }")}},
		{name: "blank var", head: fakeTree{constFile: src(decl + "\nvar _ = []int{bareSleepBaseline}")}, want: lost},
		{name: "build-tagged away", head: fakeTree{constFile: "//go:build ignore\n\n" + src(decl+"\n"+test)}, want: lost},
		{name: "test moved to a GOOS file", head: fakeTree{constFile: src(decl), dir + "sleep_linux_test.go": src(test)}, want: lost},
		{name: "test moved to another file of the package", head: fakeTree{constFile: src(decl), dir + "other_test.go": src(test)}},
		{name: "used by non-test code of the package", head: fakeTree{constFile: src(decl), dir + "rule.go": src("func over(n int) bool { return n > bareSleepBaseline }")}},
		{name: "used only in another package", head: fakeTree{constFile: src(decl), "internal/other/x_test.go": src(test)}, want: lost},
		{name: "constant and test deleted", head: fakeTree{}, want: []raise{{"go:internal/testhelper#bareSleepBaseline", 135, -1}}},
		{name: "constant renamed with its use", head: fakeTree{constFile: src(strings.ReplaceAll(decl+"\n"+test, "bareSleep", "sleep"))},
			want: []raise{{"go:internal/testhelper#bareSleepBaseline", 135, -1}}},
		{name: "raised and its use dropped", head: fakeTree{constFile: src("const bareSleepBaseline = 136")},
			want: []raise{{ref, 1, -1}, {"go:internal/testhelper#bareSleepBaseline", 135, 136}}},
		{name: "never used", base: unused, head: unused},
		{name: "used for the first time", base: unused, head: used},
		{name: "ledger line clears it", head: fakeTree{constFile: src(decl), ledgerPath: `{"gate":"` + ref + `","from":1,"to":-1,"issue":1,"reason":"r"}` + "\n"},
			want: lost, cleared: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := tc.base
			if b == nil {
				b = used
			}
			problems, rs, err := run(b, tc.head, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(rs, tc.want) {
				t.Errorf("raises = %v, want %v", rs, tc.want)
			}
			wantProblems := len(tc.want)
			if tc.cleared {
				wantProblems = 0
			}
			if len(problems) != wantProblems {
				t.Errorf("problems = %q, want %d", problems, wantProblems)
			}
		})
	}
}

// A skip in a file that declares or compares against a baseline can keep the
// comparison from ever running, so a new one there needs a ledger line; one
// elsewhere in the package does not.
func TestRun_SkipInAFileWithABaseline(t *testing.T) {
	t.Parallel()
	const (
		dir       = "internal/testhelper/"
		constFile = dir + "sleep_ratchet_test.go"
		gate      = "go-skip:internal/testhelper"
	)
	src := func(body ...string) string { return "package testhelper\n\n" + strings.Join(body, "\n") + "\n" }
	const (
		decl    = "const bareSleepBaseline = 135"
		test    = "func TestX(t *testing.T) {\n\tif n > bareSleepBaseline {\n\t\tt.Fatal()\n\t}\n}"
		skipped = "func TestY(t *testing.T) { t.Skip() }"
	)
	used := fakeTree{constFile: src(decl, test)}
	const other = dir + "other_test.go"
	testZ := strings.Replace(test, "TestX", "TestZ", 1)
	withOther := func(body string) fakeTree { return fakeTree{constFile: src(decl, test), other: body} }
	skip := []raise{{gate, 0, 1}}
	for _, tc := range []struct {
		name    string
		base    fakeTree // nil: used
		head    fakeTree
		want    []raise
		cleared bool
	}{
		{name: "unchanged", head: used},
		{name: "skip in the comparing test", head: fakeTree{constFile: src(decl, strings.Replace(test, "{\n", "{\n\tt.Skip(\"flaky\")\n", 1))}, want: skip},
		{name: "skip in another test of the file", head: fakeTree{constFile: src(decl, test, skipped)}, want: skip},
		{name: "Skipf", head: fakeTree{constFile: src(decl, test, `func TestY(t *testing.T) { t.Skipf("%d", 1) }`)}, want: skip},
		{name: "SkipNow as a method value", head: fakeTree{constFile: src(decl, test, "func TestY(t *testing.T) { s := t.SkipNow; s() }")}, want: skip},
		{name: "skip on a benchmark", head: fakeTree{constFile: src(decl, test, "func BenchmarkY(b *testing.B) { b.Skip() }")}, want: skip},
		{name: "two skips", head: fakeTree{constFile: src(decl, test, skipped, "func TestZ(t *testing.T) { t.SkipNow() }")}, want: []raise{{gate, 0, 2}}},
		{name: "skip in the file the test moved to", head: fakeTree{constFile: src(decl), dir + "other_test.go": src(test, skipped)}, want: skip},
		{name: "skip in a file with no baseline use", head: fakeTree{constFile: src(decl, test), dir + "other_test.go": src(skipped)}},
		{name: "SkipDir is not a skip", head: fakeTree{constFile: src(decl, test, "var errSkip = filepath.SkipDir")}},
		{name: "skip removed", base: fakeTree{constFile: src(decl, test, skipped)}, head: used},
		{name: "skip in the declaring file, comparison moved out", head: fakeTree{
			constFile:             src(decl, "func TestX(t *testing.T) {\n\tt.Skip(\"flaky\")\n\tcheck(t)\n}"),
			dir + "check_test.go": src(strings.Replace(test, "TestX", "check", 1)),
		}, want: skip},
		{name: "skip in a declaring file with no use", base: fakeTree{constFile: src(decl), dir + "other_test.go": src(test)},
			head: fakeTree{constFile: src(decl, skipped), dir + "other_test.go": src(test)}, want: skip},
		{name: "new baseline in a file that already skips", base: fakeTree{constFile: src(skipped)}, head: fakeTree{constFile: src(decl, test, skipped)}},
		{name: "new baseline with a new skip", base: fakeTree{}, head: fakeTree{constFile: src(decl, test, skipped)}},
		{name: "existing skip in a file that gains a use", base: withOther(src(skipped)),
			head: fakeTree{constFile: src(decl, test), other: src(skipped, testZ)}},
		{name: "existing skip in a file that gains a declaration", base: withOther(src(skipped)),
			head: fakeTree{constFile: src(decl, test), other: src("const otherBaseline = 1", skipped)}},
		{name: "file gains a use and a new skip", base: withOther(src(skipped)),
			head: fakeTree{constFile: src(decl, test), other: src(skipped, testZ, "func TestW(t *testing.T) { t.SkipNow() }")}, want: []raise{{gate, 1, 2}}},
		{name: "skip moved from an uncounted file into a counted one", base: withOther(src(skipped)),
			head: fakeTree{constFile: src(decl, test, skipped), other: src()}, want: skip},
		// Skips are counted per file, not per test.
		{name: "existing skip in a test that gains a comparison", base: withOther(src(skipped)),
			head: fakeTree{constFile: src(decl, test), other: src(strings.Replace(test, "TestX(t *testing.T) {\n", "TestY(t *testing.T) {\n\tt.Skip()\n", 1))}},
		{name: "build constraint dropped from a file with a skip", base: withOther("//go:build never\n\n" + src(skipped, testZ)),
			head: fakeTree{constFile: src(decl, test), other: src(skipped, testZ)}, want: skip},
		// The base side reads the same path, so a rename reads as a new file.
		{name: "renamed file with a skip gains a use", base: withOther(src(skipped)),
			head: fakeTree{constFile: src(decl, test), dir + "renamed_test.go": src(skipped, testZ)}, want: skip},
		{name: "ledger line clears it", head: fakeTree{constFile: src(decl, test, skipped), ledgerPath: `{"gate":"` + gate + `","from":0,"to":1,"issue":1,"reason":"r"}` + "\n"},
			want: skip, cleared: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := tc.base
			if b == nil {
				b = used
			}
			problems, rs, err := run(b, tc.head, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(rs, tc.want) {
				t.Errorf("raises = %v, want %v", rs, tc.want)
			}
			wantProblems := len(tc.want)
			if tc.cleared {
				wantProblems = 0
			}
			if len(problems) != wantProblems {
				t.Errorf("problems = %q, want %d", problems, wantProblems)
			}
		})
	}
}

// Retiring a baseline is a raise to -1 like any other; its ledger entry
// clears it.
func TestRun_DeletedGoBaselineWithLedgerEntry(t *testing.T) {
	t.Parallel()
	base := fakeTree{"internal/testhelper/sleep_ratchet_test.go": "package testhelper\nconst bareSleepBaseline = 0\n"}
	head := fakeTree{ledgerPath: `{"gate":"go:internal/testhelper#bareSleepBaseline","from":0,"to":-1,"issue":1,"reason":"replaced by a hard ban"}` + "\n"}
	problems, rs, err := run(base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || len(problems) != 0 {
		t.Errorf("raises = %v, problems = %q; want one raise, cleared", rs, problems)
	}
}

// The preselect is a fixed string every baseline name contains, so no
// spelling of the declaration keeps its file from being parsed.
func TestGitGrepArgs_FindsEverySpelling(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	files := map[string]string{
		"typed.go":     "package a\nconst xBaseline int = 1\n",
		"comment.go":   "package a\nconst yBaseline = 1 // c\n",
		"sum.go":       "package a\nconst zBaseline = 1 + 1\n",
		"block.go":     "package a\nconst (\n\tvBaseline = 1\n\twBaseline\n)\n",
		"unrelated.go": "package a\nconst budget = 1\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	cmd := gitGrepArgs("")
	got, err := grepBaselineFiles(append([]string{cmd[0], "-C", dir}, cmd[1:]...)...)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"block.go", "comment.go", "sum.go", "typed.go"}; !slices.Equal(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
}

func TestGrepPaths(t *testing.T) {
	t.Parallel()
	got := grepPaths("origin/master:internal/a_test.go\n3f2e1d:tools/x/main.go\n")
	if want := []string{"internal/a_test.go", "tools/x/main.go"}; !slices.Equal(got, want) {
		t.Errorf("with a revision: %v, want %v", got, want)
	}
	if got := grepPaths("internal/a_test.go\n"); !slices.Equal(got, []string{"internal/a_test.go"}) {
		t.Errorf("working tree: %v", got)
	}
}
