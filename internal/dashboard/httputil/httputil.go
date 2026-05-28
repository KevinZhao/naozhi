// Package httputil provides JSON-API helpers shared by the server package
// and its dashboard sub-packages.
//
// Phase 3-prep (server-split-phase4-design.md §6.5 Plan B): these helpers
// were previously private in internal/server/dashboard.go. To allow dashboard
// handlers to live in dedicated sub-packages (cron / project / discovery /
// auth / etc.) without all of them re-importing internal/server in a cycle,
// the helpers move here and gain exported names.
//
// Three layers worth of contract preserved across the move:
//
//  1. CLIENT-SIDE rendering contract — every JSON field a handler emits
//     through WriteJSON / WriteJSONStatus / MarshalPooled is rendered by
//     dashboard.js via textContent (or DOMPurify) and never assigned to
//     innerHTML. Strings carry HTML metacharacters unescaped (`<`, `>`,
//     `&`) so the contract is enforced on the consumer side; a future
//     consumer that adds `el.innerHTML = resp.content` becomes a stored-
//     XSS vector immediately. R243-SEC-10 / R245-SEC-13 / R238-SEC-5.
//
//  2. CACHE / SNIFF headers — every response sets `Cache-Control: no-store`
//     and `X-Content-Type-Options: nosniff` so shared proxies can't retain
//     authenticated payloads and legacy browsers can't MIME-sniff JSON as
//     HTML/JS. R58-PERF-001.
//
//  3. POOLED ENCODER — WriteJSON / WriteJSONStatus / MarshalPooled all share
//     a sync.Pool of *json.Encoder pre-configured with SetEscapeHTML(false).
//     The escape bit is the JSON-API contract: HTML-template render paths
//     MUST escape and MUST NOT borrow this pool. The contract is pinned by
//     TestSetEscapeHTMLFalseScopedToPackage in this package's test suite.
//     R245-SEC-13.
package httputil

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
)

// MaxRequestBodyBytes is the per-handler request-body read limit applied via
// http.MaxBytesReader. 1 MiB is well above the largest JSON payload any
// handler legitimately accepts, but safely below typical DoS-attempt sizes.
// All dashboard mutation handlers must use this constant so the limit is
// adjusted in one place.
const MaxRequestBodyBytes = 1 << 20

// jsonEncBuf pairs a pooled bytes.Buffer with a json.Encoder bound to it.
// Reused by WriteJSON/WriteJSONStatus so hot dashboard poll paths do not
// allocate one encoder per HTTP response.
type jsonEncBuf struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

// jsonEncPool produces encoders with SetEscapeHTML(false) baked in. R243-SEC-10:
// json.Encoder does not expose the escape-html bit at the type level, so there
// is no compile-time guard preventing a future caller from doing
// `e.enc.SetEscapeHTML(true)` on a borrowed encoder and silently breaking the
// CLIENT-SIDE CONTRACT documented above WriteJSON. The contract is pinned at
// test time by TestJSONEncPoolHTMLEscapingDisabled, which encodes `<>&` and
// asserts the literal bytes (not `<`/`>`/`&`) appear in the wire format. If
// you add a new code path that borrows from this pool, do NOT mutate `e.enc`
// configuration — make a fresh encoder if you need different settings, or
// extend the contract test to cover the new mode explicitly.
//
// R245-SEC-13 (#842): the SetEscapeHTML(false) literal must NOT appear in
// any other internal/dashboard/httputil source file outside this one, even
// via a hand-rolled encoder. TestSetEscapeHTMLFalseScopedToPackage scans
// every non-test .go in this package and fails CI if a fresh encoder
// elsewhere flips the bit. The same rule was previously enforced by
// TestSetEscapeHTMLFalse_ScopedToWriteJSONHelper inside internal/server;
// after the move that test scans internal/server only and asserts the
// literal is absent from server entirely.
var jsonEncPool = sync.Pool{
	New: func() any {
		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		return &jsonEncBuf{buf: buf, enc: enc}
	},
}

// jsonEncBufMaxCap caps the buffer we return to the pool so a one-off large
// response (e.g. 2MB sessions snapshot) does not permanently pin that capacity.
const jsonEncBufMaxCap = 256 * 1024

// getJSONEnc returns a pooled encoder. The returned encoder always has HTML
// escaping disabled; callers MUST NOT mutate its configuration.
func getJSONEnc() *jsonEncBuf {
	e := jsonEncPool.Get().(*jsonEncBuf)
	e.buf.Reset()
	return e
}

func putJSONEnc(e *jsonEncBuf) {
	if e.buf.Cap() > jsonEncBufMaxCap {
		return
	}
	jsonEncPool.Put(e)
}

// WriteJSON writes v as a JSON response with the standard dashboard headers:
//
//	Content-Type:           application/json
//	X-Content-Type-Options: nosniff
//	Cache-Control:          no-store
//
// CLIENT-SIDE CONTRACT (R243-SEC-10): all string fields emitted through this
// helper (e.g. `last_prompt`, `summary`, `detail`) MUST be rendered through
// `textContent` in dashboard.js, OR — if rich rendering is required — passed
// through DOMPurify / a whitelist renderer before any innerHTML assignment.
// A future consumer that adds `el.innerHTML = resp.content` without
// DOMPurify would immediately become a stored-XSS vector (file contents are
// user-writable via /api/sessions/send + tool writes). When introducing a new
// response field destined for innerHTML, route it through a dedicated helper
// or the CSP `sandbox` iframe path instead of relaxing this rule.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	e := getJSONEnc()
	defer putJSONEnc(e)
	if err := e.enc.Encode(v); err != nil {
		slog.Debug("write json response", "err", err)
		return
	}
	if _, err := w.Write(e.buf.Bytes()); err != nil {
		slog.Debug("write json response", "err", err)
	}
}

// jsonOKBody is the pre-marshaled body for the common `{"status":"ok"}`
// acknowledgement reply. 20+ dashboard endpoints used to allocate a
// `map[string]string{"status":"ok"}` + run it through the JSON encoder on every
// success response; those hot paths now call WriteOK which just copies these
// bytes verbatim (plus a trailing `\n` to match the encoder's NDJSON framing).
// R64-PERF-M4.
var jsonOKBody = []byte("{\"status\":\"ok\"}\n")

// WriteOK writes the pre-marshaled `{"status":"ok"}` body with the same headers
// as WriteJSON. Use this in preference to WriteJSON for fixed ack replies so
// success paths skip the pooled encoder dance entirely.
func WriteOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(jsonOKBody); err != nil {
		slog.Debug("write json response", "err", err)
	}
}

// WriteJSONStatus is like WriteJSON but writes a non-200 HTTP status code.
// Content-Type must be set before WriteHeader, so this helper ensures
// the correct ordering: Set header → WriteHeader → Encode body.
func WriteJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	e := getJSONEnc()
	defer putJSONEnc(e)
	if err := e.enc.Encode(v); err != nil {
		slog.Debug("write json response", "err", err)
		return
	}
	if _, err := w.Write(e.buf.Bytes()); err != nil {
		slog.Debug("write json response", "err", err)
	}
}

// ErrEmptyJSONBody is returned by DecodeJSONBody when the request has a zero-
// length body. Callers can errors.Is against it to emit a specific message
// instead of the generic JSON parse error.
var ErrEmptyJSONBody = errors.New("empty request body")

// DecodeJSONBody reads r.Body into memory and unmarshals it into dst.
//
// Callers MUST have wrapped r.Body with http.MaxBytesReader beforehand so an
// oversize client cannot force unbounded io.ReadAll; the JSON POST handlers
// in the dashboard packages all follow that pattern. RNEW-PERF-001: compared
// with json.NewDecoder(r.Body).Decode(dst), this variant avoids the 4 KiB
// bufio.Reader the stdlib Decoder wraps around every request body — bodies
// are already ≤ a few MiB and fit comfortably in a single []byte. We feed
// those bytes into a json.Decoder via bytes.Reader (no internal buffering)
// purely to enable DisallowUnknownFields below.
//
// Error semantics match Decoder.Decode closely: unmarshal errors, empty
// body (ErrEmptyJSONBody), and MaxBytesError all surface as a single error
// the caller can log/return as 400. Callers that previously wrote specific
// 413 responses from MaxBytesReader must still check errors.As against
// *http.MaxBytesError; they already do today.
//
// R20260527122801-SEC-5 (#1329): DisallowUnknownFields is set on the decoder.
// Mass-assignment hygiene — if a future patch adds a new sensitive field to
// a struct (e.g. `Privileged bool`) before the dashboard exposes it, attackers
// cannot blind-POST it through any endpoint that decodes via this helper.
// Callers receive a 400-class json error ("json: unknown field …").
func DecodeJSONBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return ErrEmptyJSONBody
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// MarshalPooled marshals v via the pooled encoder and copies the result into a
// fresh []byte. Callers who would otherwise call json.Marshal on a hot path
// (WS event fanout, session_state broadcasts) use this to avoid the per-call
// encodeState allocation. Returned slice is safe to share/outlive the pool.
//
// HTML escaping is disabled on the pooled encoder (see jsonEncPool); the same
// CLIENT-SIDE CONTRACT documented on WriteJSON applies to any string field
// carried over a MarshalPooled-encoded message: clients MUST render strings
// via textContent (or DOMPurify) and never assign them to innerHTML.
//
// R238-SEC-5 (#821): if a future consumer renders a MarshalPooled-encoded
// payload via innerHTML (without DOMPurify), the unescaped `<`/`>`/`&`
// preserved here become an XSS escalation. Such call sites MUST switch to
// MarshalEscaped instead — the contract is JSON-API-only; HTML-template render
// paths require the escaped variant.
func MarshalPooled(v any) ([]byte, error) {
	e := getJSONEnc()
	defer putJSONEnc(e)
	if err := e.enc.Encode(v); err != nil {
		return nil, err
	}
	out := make([]byte, e.buf.Len())
	copy(out, e.buf.Bytes())
	return out, nil
}

// MarshalEscaped is the HTML-safe counterpart to MarshalPooled. R238-SEC-5
// (#821) called out that the SetEscapeHTML(false) baked into jsonEncPool is
// only safe under the WriteJSON CLIENT-SIDE CONTRACT (no innerHTML without
// DOMPurify); callers who cannot guarantee that contract — e.g. payloads that
// are spliced into HTML templates, embedded inside <script type="application/
// json">, or rendered via innerHTML on any non-DOMPurify path — MUST encode
// via this helper instead.
//
// Implementation note: this is intentionally a fresh json.Encoder per call
// (not a separate sync.Pool). The expected call sites for MarshalEscaped are
// rare, off-hot-path, and already paying for HTML-template rendering; pooling
// an additional encoder type would invite the same future-mutation hazard
// without measurable payoff. If a future hot path needs the escaped form,
// introduce a second pool with its own contract test rather than reusing
// jsonEncPool.
func MarshalEscaped(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	raw := buf.Bytes()
	if n := len(raw); n > 0 && raw[n-1] == '\n' {
		raw = raw[:n-1]
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out, nil
}
