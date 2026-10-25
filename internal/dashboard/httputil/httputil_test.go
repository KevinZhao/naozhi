package httputil

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWriteOK locks the wire format of the pre-marshaled `{"status":"ok"}`
// body so refactors cannot quietly drop the trailing newline (which breaks
// bash pipelines expecting NDJSON framing) or flip the key name.
// R64-PERF-M4 regression.
func TestWriteOK(t *testing.T) {
	w := httptest.NewRecorder()
	WriteOK(w)

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

// TestWriteJSONSetsNoStoreCache locks the R58-PERF-001 contract that
// authenticated dashboard JSON responses carry Cache-Control: no-store, so no
// intermediate proxy or browser bfcache retains last_prompt / PID / cost state
// that would leak to the next user on the same cache.
func TestWriteJSONSetsNoStoreCache(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSON(w, map[string]string{"hello": "world"})

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

// TestWriteJSONStatusSetsNoStoreCache mirrors TestWriteJSONSetsNoStoreCache
// for the non-200 path; error responses can still contain auth-sensitive
// context (e.g. session keys in validation failures) and must not be cached.
func TestWriteJSONStatusSetsNoStoreCache(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSONStatus(w, 400, map[string]string{"error": "bad request"})

	if got, want := w.Code, 400; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
	if got, want := w.Header().Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
}

// TestJSONEncPoolHTMLEscapingDisabled pins the SetEscapeHTML(false) contract
// on every encoder drawn from jsonEncPool — both WriteJSON's HTTP path and
// MarshalPooled's WS fanout path — so a future caller that flips the pool
// factory or mutates a borrowed encoder fails CI rather than silently
// breaking the curl-friendliness contract documented above WriteJSON and the
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
func TestJSONEncPoolHTMLEscapingDisabled(t *testing.T) {
	probe := map[string]string{
		"v": "<a href=\"x\">&</a>",
	}
	literals := []string{"<", ">", "&"}
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

	w := httptest.NewRecorder()
	WriteJSON(w, probe)
	check("WriteJSON body", w.Body.String())

	raw, err := MarshalPooled(probe)
	if err != nil {
		t.Fatalf("MarshalPooled: %v", err)
	}
	check("MarshalPooled body", string(raw))
}

// TestMarshalEscapedHTMLEscapingEnabled pins R238-SEC-5 (#821): MarshalEscaped
// is the HTML-safe counterpart to MarshalPooled. Future call sites that splice
// JSON into HTML templates / innerHTML-without-DOMPurify paths MUST use this
// helper, and a regression that flipped its escape bit would silently re-open
// the XSS escalation that motivated providing the helper.
func TestMarshalEscapedHTMLEscapingEnabled(t *testing.T) {
	probe := map[string]string{
		"v": "<a href=\"x\">&</a>",
	}
	raw, err := MarshalEscaped(probe)
	if err != nil {
		t.Fatalf("MarshalEscaped: %v", err)
	}
	body := string(raw)

	escLT := string([]byte{'\\', 'u', '0', '0', '3', 'c'})
	escGT := string([]byte{'\\', 'u', '0', '0', '3', 'e'})
	escAmp := string([]byte{'\\', 'u', '0', '0', '2', '6'})
	for _, esc := range []string{escLT, escGT, escAmp} {
		if !strings.Contains(body, esc) {
			t.Errorf("MarshalEscaped body missing escape %q (HTML escaping unexpectedly disabled?): %q", esc, body)
		}
	}
	for _, lit := range []string{"<", ">"} {
		if strings.Contains(body, lit) {
			t.Errorf("MarshalEscaped body contains raw %q — SetEscapeHTML(true) regressed: %q", lit, body)
		}
	}
}
