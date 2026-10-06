package testhelper

// Surface ratchet for test/e2e/e2e-shim.js (#3440). Every name the shim
// exposes is a dashboard internal the Playwright suite reaches into, mirrored
// onto window as a bare global; the shim's own header says the list may only
// shrink as tests move to first-class assertions. This counts the names and
// pins the total: a new name fails here, and a removed one asks for the
// baseline to come down with it.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// e2eShimSurfaceBaseline is the number of names e2e-shim.js puts on
// window.nz.test (and so on window).
const e2eShimSurfaceBaseline = 110

// e2eShimSurfaceFloor is far below the real count; a scan that finds fewer
// names has stopped recognising the file.
const e2eShimSurfaceFloor = 50

// e2eShimNamedHelpers is how many Object.defineProperty(surface, <variable>,
// ...) sites the shim has: expose, stateField and the strict-proxy loop. Any
// other such site defines names this scanner cannot read.
const e2eShimNamedHelpers = 3

var (
	shimExposeCall   = regexp.MustCompile(`\bexpose\(\s*\w+\s*,\s*\[([^\]]*)\]\s*\)`)
	shimQuotedName   = regexp.MustCompile(`'([^']*)'`)
	shimStateField   = regexp.MustCompile(`\bstateField\(\s*'([^']+)'`)
	shimAccessor     = regexp.MustCompile(`Object\.defineProperty\(\s*surface\s*,\s*'([^']+)'`)
	shimNamedSite    = regexp.MustCompile(`Object\.defineProperty\(\s*surface\s*,\s*[A-Za-z_$]`)
	shimStrictLoop   = regexp.MustCompile(`(?s)for\s*\(\s*const\s*\[\s*\w+\s*,\s*\w+\s*\]\s*of\s*\[(.*?)\]\s*\)\s*\{[^}]*\bstrict\(`)
	shimStrictName   = regexp.MustCompile(`\[\s*'([^']+)'\s*,`)
	shimDirectWrite  = regexp.MustCompile(`\bsurface\s*(?:\.\s*[\w$]+|\[[^\]]*\])\s*=[^=]`)
	shimBulkDefine   = regexp.MustCompile(`Object\.(?:assign|defineProperties)\(\s*(?:surface|window)\b`)
	shimWindowLit    = regexp.MustCompile(`Object\.defineProperty\(\s*window\s*,\s*['"]`)
	shimWindowAssign = regexp.MustCompile(`\bwindow\s*\.\s*([\w$.]+)\s*=[^=]`)
)

// countShimSurface returns the distinct names src exposes, and the forms it
// could not count: a surface defined some other way than the shim's helpers.
func countShimSurface(src string) (names []string, problems []string) {
	seen := map[string]int{}
	add := func(n string) { seen[n]++ }
	for _, m := range shimExposeCall.FindAllStringSubmatch(src, -1) {
		for _, q := range shimQuotedName.FindAllStringSubmatch(m[1], -1) {
			add(q[1])
		}
	}
	for _, re := range []*regexp.Regexp{shimStateField, shimAccessor} {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			add(m[1])
		}
	}
	for _, m := range shimStrictLoop.FindAllStringSubmatch(src, -1) {
		for _, q := range shimStrictName.FindAllStringSubmatch(m[1], -1) {
			add(q[1])
		}
	}
	for n, c := range seen {
		names = append(names, n)
		if c > 1 {
			problems = append(problems, fmt.Sprintf("%q is defined %d times", n, c))
		}
	}
	sort.Strings(names)

	if got := len(shimNamedSite.FindAllString(src, -1)); got != e2eShimNamedHelpers {
		problems = append(problems, fmt.Sprintf("%d Object.defineProperty(surface, <variable>) sites, want %d (expose, stateField, the strict loop)", got, e2eShimNamedHelpers))
	}
	for _, re := range []*regexp.Regexp{shimDirectWrite, shimBulkDefine, shimWindowLit} {
		for _, m := range re.FindAllString(src, -1) {
			problems = append(problems, fmt.Sprintf("unrecognised surface definition %q", strings.TrimSpace(m)))
		}
	}
	for _, m := range shimWindowAssign.FindAllStringSubmatch(src, -1) {
		if m[1] != "nz.test" {
			problems = append(problems, fmt.Sprintf("unrecognised surface definition %q", strings.TrimSpace(m[0])))
		}
	}
	sort.Strings(problems)
	return names, problems
}

// e2eShimSurfaceProblems compares a count with the baseline, in either direction.
func e2eShimSurfaceProblems(n int) []string {
	switch {
	case n < e2eShimSurfaceFloor:
		return []string{fmt.Sprintf("found %d e2e-shim names, below the floor of %d: the scan no longer recognises the file", n, e2eShimSurfaceFloor)}
	case n > e2eShimSurfaceBaseline:
		return []string{fmt.Sprintf("the e2e-shim surface grew: %d > baseline %d.\n"+
			"Assert through the DOM, the mock server or a first-class hook instead of exposing another internal;\n"+
			"raising the baseline needs an approved ledger entry (scripts/ratchet-raises.jsonl)", n, e2eShimSurfaceBaseline)}
	case n < e2eShimSurfaceBaseline:
		return []string{fmt.Sprintf("the e2e-shim surface dropped to %d (baseline %d): lower e2eShimSurfaceBaseline to %d", n, e2eShimSurfaceBaseline, n)}
	}
	return nil
}

func TestE2EShimSurfaceRatchet(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "test", "e2e", "e2e-shim.js"))
	if err != nil {
		t.Fatal(err)
	}
	names, problems := countShimSurface(string(data))
	for _, p := range problems {
		t.Errorf("e2e-shim.js: %s", p)
	}
	for _, p := range e2eShimSurfaceProblems(len(names)) {
		t.Error(p)
	}
}

func TestE2EShimSurfaceProblems_BothDirections(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		n    int
		want string
	}{
		{"at baseline", e2eShimSurfaceBaseline, ""},
		{"grew", e2eShimSurfaceBaseline + 1, "surface grew"},
		{"dropped", e2eShimSurfaceBaseline - 1, fmt.Sprintf("lower e2eShimSurfaceBaseline to %d", e2eShimSurfaceBaseline-1)},
		{"below the floor", e2eShimSurfaceFloor - 1, "no longer recognises"},
	} {
		got := e2eShimSurfaceProblems(tc.n)
		if tc.want == "" {
			if len(got) != 0 {
				t.Errorf("%s: %q, want none", tc.name, got)
			}
			continue
		}
		if len(got) != 1 || !strings.Contains(got[0], tc.want) {
			t.Errorf("%s: %q, want one containing %q", tc.name, got, tc.want)
		}
	}
}

func TestCountShimSurface(t *testing.T) {
	t.Parallel()
	helpers := "function expose(mod, names) {\n" +
		"  for (const name of names) Object.defineProperty(surface, name, { get: () => mod[name] });\n}\n" +
		"function stateField(name, obj, key) {\n  Object.defineProperty(surface, name, { get: () => obj[key] });\n}\n"
	strictLoop := "for (const [name, obj] of [['wsm', wsManager.wsm], ['sessionStream', sessionStream]]) {\n" +
		"  const p = strict(obj, name);\n  Object.defineProperty(surface, name, { get: () => p });\n}\n"
	mirror := "window.nz.test = surface;\nfor (const name of Object.keys(surface)) {\n" +
		"  const d = Object.getOwnPropertyDescriptor(surface, name);\n" +
		"  Object.defineProperty(window, name, { get: d.get, set: d.set, configurable: true });\n}\n"
	clean := helpers +
		"expose(a, ['one', 'two']);\n" +
		"expose(b, [\n  'three',\n  'four',\n]);\n" +
		"stateField('five', s, 'k');\n" +
		"Object.defineProperty(surface, 'six', { get: () => x.y });\n" +
		strictLoop + mirror

	names, problems := countShimSurface(clean)
	want := []string{"five", "four", "one", "sessionStream", "six", "three", "two", "wsm"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v, want %v (state keys and module refs are not names)", names, want)
	}
	if len(problems) != 0 {
		t.Errorf("problems = %q, want none", problems)
	}

	for _, tc := range []struct {
		name, extra, want string
	}{
		{"duplicate", "stateField('one', s, 'k');\n", `"one" is defined 2 times`},
		{"direct write", "surface.seven = 7;\n", "unrecognised surface definition"},
		{"bracket write", "surface['seven'] = 7;\n", "unrecognised surface definition"},
		{"nz.test write", "window.nz.test.seven = 7;\n", "unrecognised surface definition"},
		{"Object.assign", "Object.assign(surface, { seven: 7 });\n", "unrecognised surface definition"},
		{"window global", "window.seven = 7;\n", "unrecognised surface definition"},
		{"window accessor", "Object.defineProperty(window, 'seven', { get: () => 7 });\n", "unrecognised surface definition"},
		{"new helper", "for (const k of ks) Object.defineProperty(surface, k, { get: () => 7 });\n", "sites, want 3"},
	} {
		_, got := countShimSurface(clean + tc.extra)
		if len(got) != 1 || !strings.Contains(got[0], tc.want) {
			t.Errorf("%s: problems = %q, want one containing %q", tc.name, got, tc.want)
		}
	}
}
