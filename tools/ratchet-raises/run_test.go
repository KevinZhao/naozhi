package main

import (
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
		"internal/testhelper/sleep_ratchet_test.go": "const bareSleepBaseline = 138\n",
		jsRatchetPath:  `{"a.js":{"lines":10,"maxFnLines":3}}`,
		jsDepsPath:     `{"matrix":{},"tdz":{},"typeofGuards":{},"bridgeRefs":{}}`,
		exemptionsPath: "file_size: []\nhandle_baseline: []\n",
	}
	head := fakeTree{
		"internal/testhelper/sleep_ratchet_test.go": "const bareSleepBaseline = 139\n",
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
		"go:internal/testhelper/sleep_ratchet_test.go#bareSleepBaseline",
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
