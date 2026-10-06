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
	"slices"
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

// shimForms are the only shapes the shim may use to define a name, in
// canonical form: shimShape turns each into a regexp that ignores whitespace.
// @N@ and @NAMES@ are captured, and every quoted string in a capture is a
// name; @K@ is a quoted state key and @REF@ a dotted reference, neither counted.
var shimForms = []string{
	`const surface = {};`,
	`function expose(mod, names) {
  for (const name of names) {
    if (!(name in mod)) throw new Error('e2e-shim: module does not export ' + name);
    Object.defineProperty(surface, name, { get: () => mod[name], enumerable: true, configurable: true });
  }
}`,
	`function stateField(name, obj, key) {
  Object.defineProperty(surface, name, {
    get: () => obj[key],
    set: (v) => { obj[key] = v; },
    enumerable: true,
    configurable: true,
  });
}`,
	`const strict = (obj, name) => new Proxy(obj, {`,
	`for (const [name, obj] of [@PAIRS@]) {
  const p = strict(obj, name);
  Object.defineProperty(surface, name, { get: () => p, enumerable: true, configurable: true });
}`,
	`expose(@REF@, [@NAMES@]);`,
	`stateField(@N@, @REF@, @K@);`,
	`Object.defineProperty(surface, @N@, { get: () => @REF@, enumerable: true, configurable: true });`,
	`window.nz.test = surface;
for (const name of Object.keys(surface)) {
  const d = Object.getOwnPropertyDescriptor(surface, name);
  Object.defineProperty(window, name, { get: d.get, set: d.set, configurable: true });
}`,
}

const (
	shimQ   = `(?:'[^'\\\n]*'|"[^"\\\n]*")`
	shimRef = `[\w$]+(?:\.[\w$]+)*`
)

var (
	shimPlaceholders = map[string]string{
		"@N@":     `(` + shimQ + `)`,
		"@K@":     shimQ,
		"@REF@":   shimRef,
		"@NAMES@": `((?:\s*` + shimQ + `\s*,)*\s*(?:` + shimQ + `\s*)?)`,
		"@PAIRS@": `((?:\s*\[\s*` + shimQ + `\s*,\s*` + shimRef + `\s*\]\s*,)*\s*(?:\[\s*` + shimQ + `\s*,\s*` + shimRef + `\s*\]\s*)?)`,
	}
	shimToken  = regexp.MustCompile(`@[A-Z]+@|'[^']*'|[\w$]+|\S`)
	shimQuoted = regexp.MustCompile(`'([^'\\\n]*)'|"([^"\\\n]*)"`)
	// shimSensitive are the identifiers through which a name can reach
	// window.nz.test or window; any left once shimForms are consumed is a
	// definition the count cannot see.
	shimSensitive = regexp.MustCompile(`\b(?:surface|window|globalThis|self|top|parent|frames|defaultView|Reflect|eval|Function|Object\s*\.\s*(?:assign|defineProperty|defineProperties|setPrototypeOf)|expose|stateField|strict)\b`)
	shimFormRes   = func() []*regexp.Regexp {
		var res []*regexp.Regexp
		for _, f := range shimForms {
			res = append(res, shimShape(f))
		}
		return res
	}()
)

// shimShape compiles a canonical form, allowing any whitespace between tokens.
func shimShape(canon string) *regexp.Regexp {
	var parts []string
	for _, tok := range shimToken.FindAllString(canon, -1) {
		if re, ok := shimPlaceholders[tok]; ok {
			parts = append(parts, re)
		} else {
			parts = append(parts, regexp.QuoteMeta(tok))
		}
	}
	return regexp.MustCompile(`\b` + strings.Join(parts, `\s*`))
}

// blankJS replaces comments, and with blankStrings the contents of ” and ""
// literals, by spaces, keeping newlines so line numbers hold. Template
// literals keep their contents, since ${} can hold code. Regex literals are
// not recognised; the shim has none.
func blankJS(src string, blankStrings bool) string {
	out := []byte(src)
	for i := 0; i < len(src); {
		switch c := src[i]; {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			j := i
			for j < len(src) && src[j] != '\n' {
				j++
			}
			blankBytes(out, i, j)
			i = j
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			j := len(src)
			if e := strings.Index(src[i+2:], "*/"); e >= 0 {
				j = i + 2 + e + 2
			}
			blankBytes(out, i, j)
			i = j
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if blankStrings && c != '`' {
				blankBytes(out, i+1, j)
			}
			i = j + 1
		default:
			i++
		}
	}
	return string(out)
}

// blankBytes spaces out b[from:to], keeping newlines.
func blankBytes(b []byte, from, to int) {
	for k := from; k < to && k < len(b); k++ {
		if b[k] != '\n' {
			b[k] = ' '
		}
	}
}

// countShimSurface returns the distinct names src exposes, and the problems:
// a name defined twice, or a sensitive identifier outside every shimForms
// match, which is a definition the count cannot read.
func countShimSurface(src string) (names []string, problems []string) {
	code := []byte(blankJS(src, false))
	seen := map[string]int{}
	for _, re := range shimFormRes {
		for _, m := range re.FindAllSubmatchIndex(code, -1) {
			for g := 2; g < len(m); g += 2 {
				for _, q := range shimQuoted.FindAllSubmatch(code[m[g]:m[g+1]], -1) {
					seen[string(q[1])+string(q[2])]++
				}
			}
			blankBytes(code, m[0], m[1])
		}
	}
	for n, c := range seen {
		names = append(names, n)
		if c > 1 {
			problems = append(problems, fmt.Sprintf("%q is defined %d times", n, c))
		}
	}
	sort.Strings(names)
	sort.Strings(problems)

	srcLines := strings.Split(src, "\n")
	for i, line := range strings.Split(blankJS(string(code), true), "\n") {
		if ids := shimSensitive.FindAllString(line, -1); len(ids) > 0 {
			problems = append(problems, fmt.Sprintf("line %d: unrecognised surface definition (%s): %s",
				i+1, strings.Join(ids, ", "), strings.TrimSpace(srcLines[i])))
		}
	}
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
	// The placeholder-free forms are the helpers and the window mirror, as the
	// real shim spells them.
	var fixed []string
	for _, f := range shimForms {
		if !strings.Contains(f, "@") {
			fixed = append(fixed, f)
		}
	}
	clean := "// header: window.nz.test surface, expose(x, more)\n/**\n * stateField(name) puts it on window\n */\n" +
		strings.Join(fixed, "\n  get(t, k) { return t[k]; },\n});\n") + "\n" +
		"expose(a, ['one', \"two\"]);\n" +
		"expose(b, [\n  'three',\n  'four', // window\n]);\n" +
		"stateField('five', s.t, 'k');\n" +
		"stateField(\"seven\", s, \"k2\");\n" +
		"Object.defineProperty(surface, 'six', { get: () => x.y, enumerable: true, configurable: true });\n" +
		"for (const [name, obj] of [['wsm', wsManager.wsm], ['sessionStream', sessionStream]]) {\n" +
		"  const p = strict(obj, name);\n" +
		"  Object.defineProperty(surface, name, { get: () => p, enumerable: true, configurable: true });\n}\n" +
		"const msg = 'self top parent';\n"

	names, problems := countShimSurface(clean)
	want := []string{"five", "four", "one", "sessionStream", "seven", "six", "three", "two", "wsm"}
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
		{"direct write", "surface.eight = 8;\n", "(surface)"},
		{"bracket write", "surface['eight'] = 8;\n", "(surface)"},
		{"nz.test write", "window.nz.test.eight = 8;\n", "(window)"},
		{"Object.assign", "Object.assign(surface, { eight: 8 });\n", "(Object.assign, surface)"},
		{"window global", "window.eight = 8;\n", "(window)"},
		{"window bracket", "window['eight'] = 8;\n", "(window)"},
		{"globalThis", "globalThis.eight = 8;\n", "(globalThis)"},
		{"self", "self.eight = 8;\n", "(self)"},
		{"window accessor", "Object.defineProperty(window, 'eight', { get: () => 8 });\n", "(Object.defineProperty, window)"},
		{"Reflect", "Reflect.defineProperty(surface, 'eight', { get: () => 8 });\n", "(Reflect, surface)"},
		{"double-quoted accessor, other shape", "Object.defineProperty(surface, \"eight\", { get: () => 8 });\n", "(Object.defineProperty, surface)"},
		{"template-literal name", "stateField(`eight`, s, 'k');\n", "(stateField)"},
		{"new helper", "for (const k of ks) Object.defineProperty(surface, k, { get: () => 8 });\n", "(Object.defineProperty, surface)"},
		{"non-literal array", "const more = ['eight'];\nexpose(nzUtil, more);\n", "(expose)"},
		{"computed array", "expose(nzUtil, Object.keys(nzUtil).filter((n) => n));\n", "(expose)"},
		{"array element not a string", "expose(nzUtil, ['eight', n]);\n", "(expose)"},
		{"expose in a loop", "for (const n of ['eight']) expose(nzUtil, [n]);\n", "(expose)"},
		{"aliased helper", "const e = expose;\n", "(expose)"},
		{"helper reshaped", "", "(expose)"},
		{"getter does more", "Object.defineProperty(surface, 'eight', { get: () => (window.x = 1), enumerable: true, configurable: true });\n", "(Object.defineProperty, surface, window)"},
		{"template interpolation", "const s = `${window.eight = 8}`;\n", "(window)"},
	} {
		src := clean + tc.extra
		if tc.name == "helper reshaped" {
			src = strings.Replace(src, "for (const name of names)", "for (const name of [...names, 'eight'])", 1)
		}
		_, got := countShimSurface(src)
		if !slices.ContainsFunc(got, func(p string) bool { return strings.Contains(p, tc.want) }) {
			t.Errorf("%s: problems = %q, want one containing %q", tc.name, got, tc.want)
		}
	}
}
