package server

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestWriteOK locks the wire format of the pre-marshaled `{"status":"ok"}`
// body so refactors cannot quietly drop the trailing newline (which breaks
// bash pipelines expecting NDJSON framing) or flip the key name.
// R64-PERF-M4 regression.
func TestWriteOK(t *testing.T) {
	w := httptest.NewRecorder()
	writeOK(w)

	if got, want := w.Code, 200; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
	if got, want := w.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := w.Header().Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, want)
	}
	if got, want := w.Header().Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	body := w.Body.String()
	if !strings.HasSuffix(body, "\n") {
		t.Errorf("body missing trailing newline: %q", body)
	}
	if trimmed := strings.TrimSuffix(body, "\n"); trimmed != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", trimmed, `{"status":"ok"}`)
	}
}

// TestWriteJSON_SetsNoStoreCache locks the R58-PERF-001 contract that
// authenticated dashboard JSON responses carry Cache-Control: no-store, so no
// intermediate proxy or browser bfcache retains last_prompt / PID / cost state
// that would leak to the next user on the same cache. Exercised via writeJSON
// (a small opaque payload is enough — we only care about headers).
func TestWriteJSON_SetsNoStoreCache(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, map[string]string{"hello": "world"})

	if got, want := w.Header().Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	if got, want := w.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := w.Header().Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, want)
	}
}

// TestWriteJSONStatus_SetsNoStoreCache mirrors TestWriteJSON_SetsNoStoreCache
// for the non-200 path; error responses can still contain auth-sensitive
// context (e.g. session keys in validation failures) and must not be cached.
func TestWriteJSONStatus_SetsNoStoreCache(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSONStatus(w, 400, map[string]string{"error": "bad request"})

	if got, want := w.Code, 400; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
	if got, want := w.Header().Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
}

// TestJSONEncPool_HTMLEscapingDisabled pins the SetEscapeHTML(false) contract
// on every encoder drawn from jsonEncPool — both writeJSON's HTTP path and
// marshalPooled's WS fanout path — so a future caller that flips the pool
// factory or mutates a borrowed encoder fails CI rather than silently
// breaking the curl-friendliness contract documented above writeJSON and the
// dashboard-renderer expectation that `<` / `>` / `&` arrive literally so
// textContent / DOMPurify can guard them at render time.
//
// Probe carries the chars `SetEscapeHTML` controls (`<`, `>`, `&`).
// Asserts the literal bytes are in the wire and none of the `\uXXXX`
// escape forms leaked through. (LS/PS U+2028/U+2029 are deliberately
// excluded — encoding/json always escapes those regardless of
// SetEscapeHTML, and a test that forbade their escapes would fail under
// every supported Go version.)
//
// R243-SEC-10: jsonEncPool is configured at sync.Pool init and there is no
// compile-time guard against a future change relaxing the bit; this test is
// the contract pin.
func TestJSONEncPool_HTMLEscapingDisabled(t *testing.T) {
	probe := map[string]string{
		"v": "<a href=\"x\">&</a>",
	}
	literals := []string{"<", ">", "&"}
	// JSON `\uXXXX` escape forms — built via byte literals so the asserted
	// strings are guaranteed to be 6-byte ASCII sequences (backslash, 'u',
	// 4 hex digits) rather than their rendered runes. If we wrote the
	// literal in source, a tool that decodes Go escapes would turn it back
	// into the rune and the assertion would silently invert.
	escLT := string([]byte{'\\', 'u', '0', '0', '3', 'c'})
	escGT := string([]byte{'\\', 'u', '0', '0', '3', 'e'})
	escAmp := string([]byte{'\\', 'u', '0', '0', '2', '6'})
	escaped := []string{escLT, escGT, escAmp}

	check := func(label, body string) {
		for _, lit := range literals {
			if !strings.Contains(body, lit) {
				t.Errorf("%s missing literal %q (HTML escaping unexpectedly enabled?): %q", label, lit, body)
			}
		}
		for _, esc := range escaped {
			if strings.Contains(body, esc) {
				t.Errorf("%s contains escaped form %q — SetEscapeHTML(false) regressed: %q", label, esc, body)
			}
		}
	}

	// writeJSON path.
	w := httptest.NewRecorder()
	writeJSON(w, probe)
	check("writeJSON body", w.Body.String())

	// marshalPooled path (drains the same pool; used by WS fanout).
	raw, err := marshalPooled(probe)
	if err != nil {
		t.Fatalf("marshalPooled: %v", err)
	}
	check("marshalPooled body", string(raw))
}

// TestSetEscapeHTMLFalse_ScopedToWriteJSONHelper pins R245-SEC-13 (#842):
// inside internal/server, the SetEscapeHTML(false) call site is a JSON-API
// contract that must live exclusively next to writeJSON / writeJSONStatus /
// writeOK in dashboard.go. A future endpoint that hand-rolls its own pooled
// encoder and flips the bit on an HTML-template render path would re-open
// the REPEAT-2 escalation chain (raw `<` / `>` / `&` flowing into innerHTML
// without DOMPurify); fail the build instead.
//
// The check scans non-test .go files in the package directory directly,
// stripping line and block comments before substring-matching so the many
// existing godoc references to the contract (project_files.go, wshub_*,
// dashboard_send.go) don't trip the lint. Tests are excluded so the probe
// in TestJSONEncPool_HTMLEscapingDisabled can mention the symbol freely.
func TestSetEscapeHTMLFalse_ScopedToWriteJSONHelper(t *testing.T) {
	const allowedFile = "dashboard.go"
	const needle = "SetEscapeHTML(false)"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == allowedFile {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		if containsCodeOccurrence(string(body), needle) {
			t.Errorf("%s contains SetEscapeHTML(false) in code; the contract pins this call to %s "+
				"(writeJSON / marshalPooled). HTML-template render paths must escape; if a new "+
				"JSON-API helper truly needs the unescaped form, host it in dashboard.go alongside "+
				"the existing helpers and update this test's allowed-file list with the rationale.",
				name, allowedFile)
		}
	}
}

// containsCodeOccurrence reports whether needle appears in src outside any
// // line comment or /* … */ block comment. Source-text (not Go AST) so
// the test stays trivially auditable; the comment stripper handles the
// in-tree shapes (line comments at any column; block comments span lines)
// without pulling in go/parser.
func containsCodeOccurrence(src, needle string) bool {
	var b strings.Builder
	b.Grow(len(src))
	i := 0
	inBlockComment := false
	inString := false
	stringQuote := byte(0)
	for i < len(src) {
		if inBlockComment {
			if i+1 < len(src) && src[i] == '*' && src[i+1] == '/' {
				inBlockComment = false
				i += 2
				continue
			}
			i++
			continue
		}
		if inString {
			b.WriteByte(src[i])
			if src[i] == '\\' && i+1 < len(src) {
				b.WriteByte(src[i+1])
				i += 2
				continue
			}
			if src[i] == stringQuote {
				inString = false
			}
			i++
			continue
		}
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '/' {
			// Line comment: skip to end of line, preserve the newline so
			// line counts (and any subsequent code on the next line) line up.
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '*' {
			inBlockComment = true
			i += 2
			continue
		}
		if src[i] == '"' || src[i] == '`' {
			inString = true
			stringQuote = src[i]
		}
		b.WriteByte(src[i])
		i++
	}
	return strings.Contains(b.String(), needle)
}

// TestJSONEncPool_RuntimeReassertsEscapeHTMLFalse pins the R238-SEC-5 (#821)
// contract that getJSONEnc re-asserts SetEscapeHTML(false) on every Get, so
// a misbehaving caller that briefly flips the bit to `true` and forgets to
// restore it cannot poison the next borrower. Without the runtime re-assert,
// the SetEscapeHTML(false) contract was a one-time pool-init invariant only —
// fragile against any future code path that mutates a borrowed encoder.
func TestJSONEncPool_RuntimeReassertsEscapeHTMLFalse(t *testing.T) {
	probe := map[string]string{"v": "<b>&</b>"}
	escLT := string([]byte{'\\', 'u', '0', '0', '3', 'c'})

	// Step 1: poison a pooled encoder by flipping the bit and putting it back.
	e := getJSONEnc()
	e.enc.SetEscapeHTML(true)
	putJSONEnc(e)

	// Step 2: even after pulling many encoders (sync.Pool ordering is
	// non-deterministic), every borrowed encoder must produce unescaped
	// output. Loop a few times so we exercise both the same poisoned slot
	// and any newly-created ones.
	for i := 0; i < 8; i++ {
		raw, err := marshalPooled(probe)
		if err != nil {
			t.Fatalf("marshalPooled iter %d: %v", i, err)
		}
		if strings.Contains(string(raw), escLT) {
			t.Fatalf("iter %d: pool returned an encoder still in SetEscapeHTML(true) state — runtime re-assert regressed: %q", i, raw)
		}
	}
}
