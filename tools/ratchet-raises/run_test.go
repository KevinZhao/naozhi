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
		jsRatchetPath:  `{"a.js":{"lines":10,"maxFunctionLines":3}}`,
		jsDepsPath:     `{"matrix":{},"tdz":{},"typeofGuards":{},"bridgeRefs":{}}`,
		exemptionsPath: "file_size: []\nhandle_baseline: []\n",
	}
	head := fakeTree{
		"internal/testhelper/sleep_ratchet_test.go": "const bareSleepBaseline = 139\n",
		jsRatchetPath:  `{"a.js":{"lines":11,"maxFunctionLines":3}}`,
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
