package server

import (
	"encoding/json"
	"os"
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
	return runNodeEnv(t, script, nil)
}

// runNodeEnv is runNode with extra KEY=VALUE environment entries (e.g. TZ)
// appended to the inherited environment.
func runNodeEnv(t *testing.T, script string, extraEnv []string) string {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping JS behavioural test")
	}
	cmd := exec.Command(nodeBin, "-")
	cmd.Stdin = strings.NewReader(script)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	// Capture stdout alone: node prints warnings (e.g. ExperimentalWarning)
	// on stderr, which would corrupt the JSON payload under CombinedOutput.
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = exitErr.Stderr
		}
		t.Fatalf("node failed: %v\n%s", err, stderr)
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

// These two are cross-layer reconciliation between the JS key builder and the Go
// validator, which is the class #2547 lists as worth keeping: the JS side has no
// enumerator Go can call, and the check is cheap and catches real drift. Both
// assert on values, not on how the code is spelled — one parses the character
// classes sanitizeKeySlug actually strips, the other runs the real JS under node
// and feeds its output to session.ValidateSessionKey.
//
// The other half of the retired static_palette_2429_test.go was two substring
// checks on auth_modal.js (path-highlight ranges, hover wiring). Both are
// observable, so they moved to test/e2e/project_palette.test.js; the probes
// recorded there show one of them the anchor could not see at all.
//
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
