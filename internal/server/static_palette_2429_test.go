package server

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// Regression tests for #2429 (command palette P2 items):
//
//  1. hover must move the keyboard cursor (state.activeIdx), not just the
//     .active class, or Enter opens a different row than the highlighted one;
//  2. path highlight ranges must be computed on the rendered (shortPath)
//     string, not the raw path, or every <mark> is shifted / out of range;
//  3. sanitizeKeySlug must strip every codepoint session.ValidateSessionKey
//     rejects, or a project directory with e.g. a zero-width space yields a
//     key the server 400s on first send.
//
// The behavioural tests run the extracted pure functions under node; they
// skip when node is not on PATH. The static contract tests always run.

// extractJSFunction returns the source of a top-level `function name(` up to
// and including its closing `}` at column 0.
func extractJSFunction(t *testing.T, js, name string) string {
	t.Helper()
	marker := "\nfunction " + name + "("
	i := strings.Index(js, marker)
	if i < 0 {
		t.Fatalf("dashboard.js: function %s not found", name)
	}
	rest := js[i+1:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("dashboard.js: function %s has no column-0 closing brace", name)
	}
	return rest[:end+2] + "\n"
}

// runNode executes script with node and returns its stdout. Skips when node
// is unavailable.
func runNode(t *testing.T, script string) string {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping JS behavioural test")
	}
	cmd := exec.Command(nodeBin, "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	return string(out)
}

// readDashboardJS is shared with static_event_uuid_dedup_test.go.

// parseJSCharClasses collects every codepoint covered by the character
// classes of the `.replace(/[...]/g, ...)` calls inside body. It understands
// \xHH, \uHHHH, single-char escapes (skipped, e.g. \s \\ \/), literal
// characters, and A-B ranges between any two of those.
func parseJSCharClasses(t *testing.T, body string) map[rune]bool {
	t.Helper()
	covered := map[rune]bool{}
	rest := body
	for {
		start := strings.Index(rest, ".replace(/[")
		if start < 0 {
			break
		}
		rest = rest[start+len(".replace(/["):]
		// Classes end in either `]/g` or (with a quantifier) `]+/g`.
		end := -1
		for _, term := range []string{"]/g", "]+/g"} {
			if i := strings.Index(rest, term); i >= 0 && (end < 0 || i < end) {
				end = i
			}
		}
		if end < 0 {
			t.Fatalf("unterminated character class in: %.60q", rest)
		}
		class := rest[:end]
		rest = rest[end:]

		// Tokenise into codepoints (or -1 for a non-codepoint escape like \s).
		var toks []rune
		var isDash []bool
		for i := 0; i < len(class); {
			c := class[i]
			switch {
			case c == '\\' && i+1 < len(class):
				n := class[i+1]
				switch {
				case n == 'x' && i+4 <= len(class):
					v, err := strconv.ParseUint(class[i+2:i+4], 16, 32)
					if err != nil {
						t.Fatalf("bad \\x escape in class %q", class)
					}
					toks = append(toks, rune(v))
					isDash = append(isDash, false)
					i += 4
				case n == 'u' && i+6 <= len(class):
					v, err := strconv.ParseUint(class[i+2:i+6], 16, 32)
					if err != nil {
						t.Fatalf("bad \\u escape in class %q", class)
					}
					toks = append(toks, rune(v))
					isDash = append(isDash, false)
					i += 6
				default:
					// \s, \\, \/, \? etc. — not a single codepoint we track.
					toks = append(toks, -1)
					isDash = append(isDash, false)
					i += 2
				}
			case c == '-':
				toks = append(toks, '-')
				isDash = append(isDash, true)
				i++
			default:
				r, size := decodeRune(class[i:])
				toks = append(toks, r)
				isDash = append(isDash, false)
				i += size
			}
		}
		for i := 0; i < len(toks); i++ {
			if isDash[i] && i > 0 && i+1 < len(toks) && toks[i-1] >= 0 && toks[i+1] >= 0 && !isDash[i-1] && !isDash[i+1] {
				for r := toks[i-1]; r <= toks[i+1]; r++ {
					covered[r] = true
				}
				i++ // consume range end
				continue
			}
			if toks[i] >= 0 && !isDash[i] {
				covered[toks[i]] = true
			}
		}
	}
	return covered
}

func decodeRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return -1, 1
}

// TestDashboardJS_SanitizeKeySlug_CoversServerDenySet is the cross-layer
// contract: every codepoint session.ValidateSessionKey rejects must appear
// in one of sanitizeKeySlug's strip classes. Static parse, always runs.
func TestDashboardJS_SanitizeKeySlug_CoversServerDenySet(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	body := extractJSFunction(t, js, "sanitizeKeySlug")
	covered := parseJSCharClasses(t, body)
	if len(covered) == 0 {
		t.Fatal("no character classes parsed from sanitizeKeySlug")
	}
	for _, rg := range session.DeniedKeyRuneRanges() {
		for r := rg[0]; r <= rg[1]; r++ {
			if !covered[r] {
				t.Errorf("sanitizeKeySlug does not strip U+%04X but session.ValidateSessionKey rejects it — a project dir containing it would 400 on first send (#2429)", r)
			}
		}
	}
}

// TestDashboardJS_SanitizeKeySlug_OutputPassesValidateSessionKey feeds
// every server-denied codepoint through the real JS key builder and asserts
// the Go validator accepts the result. Behavioural counterpart of the
// static parse above.
func TestDashboardJS_SanitizeKeySlug_OutputPassesValidateSessionKey(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	var cps []int
	for _, rg := range session.DeniedKeyRuneRanges() {
		for r := rg[0]; r <= rg[1]; r++ {
			cps = append(cps, int(r))
		}
	}
	cpJSON, _ := json.Marshal(cps)
	script := extractJSFunction(t, js, "sanitizeKeySlug") +
		extractJSFunction(t, js, "buildDashboardSessionKey") +
		"const cps = " + string(cpJSON) + ";\n" +
		"const out = cps.map(cp => buildDashboardSessionKey('1700000000000', 'proj' + String.fromCodePoint(cp) + 'dir', 'general'));\n" +
		"process.stdout.write(JSON.stringify(out));\n"
	var keys []string
	if err := json.Unmarshal([]byte(runNode(t, script)), &keys); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	if len(keys) != len(cps) {
		t.Fatalf("got %d keys for %d codepoints", len(keys), len(cps))
	}
	for i, k := range keys {
		if err := session.ValidateSessionKey(k); err != nil {
			t.Errorf("U+%04X: client key %q rejected by server: %v", cps[i], k, err)
		}
		if strings.ContainsRune(k, rune(cps[i])) {
			t.Errorf("U+%04X survived sanitizeKeySlug in %q", cps[i], k)
		}
	}
	// Sanity: the slug is still readable — the codepoint is dropped, not
	// dashed, so 'proj<zwsp>dir' collapses to 'projdir'.
	if !strings.Contains(keys[len(keys)-1], "-projdir:") {
		t.Errorf("BOM case: want slug 'projdir' in %q", keys[len(keys)-1])
	}
}

// TestDashboardJS_PalettePathHighlight_UsesRenderedPath: the ranges used to
// paint <mark> into the palette's path line must index into the string that
// is actually rendered (shortPath), and a query that only matches the
// collapsed full path must still keep the row (without highlight).
func TestDashboardJS_PalettePathHighlight_UsesRenderedPath(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	// Static contract: renderPaletteList must not compute path ranges on
	// the raw path any more.
	render := extractJSFunction(t, js, "renderPaletteList")
	if strings.Contains(render, "fuzzyMatch(q, p.path)") {
		t.Error("renderPaletteList still calls fuzzyMatch(q, p.path); ranges must be computed against shortPath(p.path) (matchProjectPath) — #2429")
	}
	if !strings.Contains(render, "matchProjectPath(q, p.path)") {
		t.Error("renderPaletteList must route path matching through matchProjectPath")
	}
	row := extractJSFunction(t, js, "buildProjectRow")
	if !strings.Contains(row, "highlight(shortPath(p.path), s.pathRanges)") {
		t.Fatal("buildProjectRow no longer highlights shortPath(p.path) with s.pathRanges; update this test's premise")
	}

	script := "function esc(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');}\n" +
		extractJSFunction(t, js, "fuzzyMatch") +
		extractJSFunction(t, js, "shortPath") +
		extractJSFunction(t, js, "highlight") +
		extractJSFunction(t, js, "matchProjectPath") + `
const cases = [
  ['work', '/home/ec2-user/workspace/naozhi'],
  ['naozhi', '/Users/kevin/dev/naozhi'],
  ['wsnz', '/home/ec2-user/workspace/naozhi'],
  ['tail', '/opt/very/long/prefix/that/exceeds/forty/chars/tail'],
];
const out = cases.map(([q, p]) => {
  const text = shortPath(p);
  const m = matchProjectPath(q, p);
  return {
    q, text,
    matched: !!m,
    ranges: m ? m.ranges : null,
    html: m ? highlight(text, m.ranges) : null,
    marked: m ? m.ranges.map(([s, e]) => text.substring(s, e)).join('') : null,
  };
});
const onlyFull = matchProjectPath('ec2-user', '/home/ec2-user/workspace/naozhi');
const none = matchProjectPath('zzz', '/home/ec2-user/workspace/naozhi');
process.stdout.write(JSON.stringify({out, onlyFull, none}));
`
	var res struct {
		Out []struct {
			Q, Text string
			Matched bool
			Ranges  [][2]int
			HTML    string `json:"html"`
			Marked  string
		}
		OnlyFull *struct {
			Score  int
			Ranges [][2]int
		}
		None *struct{}
	}
	if err := json.Unmarshal([]byte(runNode(t, script)), &res); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	for _, c := range res.Out {
		if !c.Matched {
			t.Errorf("q=%q text=%q: expected a match", c.Q, c.Text)
			continue
		}
		for _, r := range c.Ranges {
			if r[0] < 0 || r[1] > len(c.Text) || r[0] >= r[1] {
				t.Errorf("q=%q: range %v out of bounds for rendered text %q (len %d)", c.Q, r, c.Text, len(c.Text))
			}
		}
		if strings.ToLower(c.Marked) != c.Q {
			t.Errorf("q=%q: highlighted characters spell %q, want the query — ranges are misaligned with the rendered path %q", c.Q, c.Marked, c.Text)
		}
		if strings.Contains(c.Q, "work") && !strings.Contains(c.HTML, "<mark>work</mark>") {
			t.Errorf("q=%q: html %q lacks <mark>work</mark>", c.Q, c.HTML)
		}
	}
	if res.OnlyFull == nil {
		t.Error("query matching only the collapsed /home/<user> prefix must still keep the row (score from full path)")
	} else if len(res.OnlyFull.Ranges) != 0 {
		t.Errorf("full-path-only match must not carry ranges (they would index the raw path): %v", res.OnlyFull.Ranges)
	}
	if res.None != nil {
		t.Error("non-matching query must return null")
	}
}

// TestDashboardJS_PaletteHover_MovesKeyboardCursor: hovering a row (which
// calls setActiveIdx) must update state.activeIdx so Enter opens the hovered
// row. Runs the real setActiveIdx + handlePaletteKey under node with a
// minimal document stub.
func TestDashboardJS_PaletteHover_MovesKeyboardCursor(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	// Static contract: every hover handler must go through the state-aware
	// setActiveIdx(state, idx); the DOM-only setActiveIdx(idx) form is the bug.
	if strings.Contains(js, "setActiveIdx(idx))") {
		t.Error("row builders still call the DOM-only setActiveIdx(idx) on mouseenter — hover must also write state.activeIdx (#2429)")
	}
	render := extractJSFunction(t, js, "renderPaletteList")
	if c := strings.Count(render, "'mouseenter', () => setActiveIdx(state, "); c < 2 {
		t.Errorf("renderPaletteList must wire mouseenter -> setActiveIdx(state, i) for both the list rows and the empty-state custom row (found %d)", c)
	}
	setter := extractJSFunction(t, js, "setActiveIdx")
	if !strings.HasPrefix(setter, "function setActiveIdx(state, idx)") || !strings.Contains(setter, "state.activeIdx = idx;") {
		t.Errorf("setActiveIdx must take (state, idx) and assign state.activeIdx:\n%s", setter)
	}

	script := `
const rows = [0, 1, 2, 3].map(i => ({dataset: {idx: String(i)}, cls: new Set(),
  classList: {toggle(c, on) { on ? this._s.add(c) : this._s.delete(c); }, _s: null}}));
rows.forEach(r => { r.classList._s = r.cls; });
const overlay = {querySelectorAll: () => rows, querySelector: () => null, remove() {}};
globalThis.document = {querySelector: sel => sel === '.cmd-palette-overlay' ? overlay : null};
const picked = [];
function pickPaletteQuick() { picked.push('quick'); }
function pickPaletteProject(p) { picked.push(p.name); }
function pickPaletteCustom(v) { picked.push('custom:' + v); }
` + extractJSFunction(t, js, "setActiveIdx") + extractJSFunction(t, js, "updateActiveRow") + extractJSFunction(t, js, "handlePaletteKey") + `
const state = {overlay, activeIdx: 0, items: [
  {type: 'quick'},
  {type: 'project', data: {project: {name: 'alpha'}}},
  {type: 'project', data: {project: {name: 'beta'}}},
  {type: 'custom', query: ''},
]};
const input = {value: ''};
const ev = key => ({key, preventDefault() {}});
// Mouse moves to the third row (index 2), then Enter.
setActiveIdx(state, 2);
const activeAfterHover = rows.filter(r => r.cls.has('active')).map(r => r.dataset.idx);
handlePaletteKey(ev('Enter'), state, input);
// Keyboard still works after hover: ArrowUp from 2 -> 1, Enter.
handlePaletteKey(ev('ArrowUp'), state, input);
handlePaletteKey(ev('Enter'), state, input);
process.stdout.write(JSON.stringify({activeIdx: state.activeIdx, activeAfterHover, picked}));
`
	var res struct {
		ActiveIdx        int
		ActiveAfterHover []string
		Picked           []string
	}
	if err := json.Unmarshal([]byte(runNode(t, script)), &res); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	if fmt.Sprint(res.ActiveAfterHover) != "[2]" {
		t.Errorf("hover must paint exactly row 2 active, got %v", res.ActiveAfterHover)
	}
	if want := []string{"beta", "alpha"}; fmt.Sprint(res.Picked) != fmt.Sprint(want) {
		t.Errorf("Enter after hovering row 2 then ArrowUp: picked %v, want %v (Enter must open the hovered row, not the stale activeIdx)", res.Picked, want)
	}
	if res.ActiveIdx != 1 {
		t.Errorf("state.activeIdx = %d after ArrowUp from hovered row 2, want 1", res.ActiveIdx)
	}
}
