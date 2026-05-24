package server

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
