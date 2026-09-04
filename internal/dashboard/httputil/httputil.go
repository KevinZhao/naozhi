// Package httputil provides JSON-API helpers shared by the server package
// and its dashboard sub-packages.
//
// Contracts:
//
//  1. CLIENT-SIDE rendering — every JSON string a handler emits through
//     WriteJSON / WriteJSONStatus / MarshalPooled is rendered by dashboard.js
//     via textContent (or DOMPurify), never innerHTML. HTML metacharacters
//     are left unescaped, so a consumer that adds `el.innerHTML = resp.x`
//     becomes a stored-XSS vector immediately.
//
//  2. CACHE / SNIFF headers — every response sets `Cache-Control: no-store`
//     and `X-Content-Type-Options: nosniff`.
//
//  3. POOLED ENCODER — the shared *json.Encoder pool has SetEscapeHTML(false);
//     HTML-template render paths MUST NOT borrow it (pinned by
//     TestSetEscapeHTMLFalseScopedToPackage).
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
// http.MaxBytesReader. All dashboard mutation handlers must use it so the
// limit is adjusted in one place.
const MaxRequestBodyBytes = 1 << 20

// jsonEncBuf pairs a pooled bytes.Buffer with a json.Encoder bound to it so
// hot dashboard poll paths do not allocate one encoder per response.
type jsonEncBuf struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

// jsonEncPool produces encoders with SetEscapeHTML(false) baked in. Callers
// that borrow from this pool MUST NOT mutate the encoder configuration; make
// a fresh encoder if different settings are needed. The SetEscapeHTML(false)
// literal must not appear in any other file of this package (#842); both
// rules are pinned by contract tests.
var jsonEncPool = sync.Pool{
	New: func() any {
		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		return &jsonEncBuf{buf: buf, enc: enc}
	},
}

// jsonEncBufMaxCap caps buffers returned to the pool so a one-off large
// response does not permanently pin that capacity.
const jsonEncBufMaxCap = 256 * 1024

// getJSONEnc returns a pooled encoder (HTML escaping disabled; do not mutate).
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

// WriteJSON writes v as a JSON response with the standard dashboard headers
// (Content-Type: application/json, X-Content-Type-Options: nosniff,
// Cache-Control: no-store).
//
// CLIENT-SIDE CONTRACT: every string field emitted through this helper MUST
// be rendered via textContent in dashboard.js, or through DOMPurify before
// any innerHTML assignment. Fields destined for innerHTML need a dedicated
// helper or the CSP `sandbox` iframe path instead of relaxing this rule.
func WriteJSON(w http.ResponseWriter, v any) {
	WriteJSONStatus(w, http.StatusOK, v)
}

// encodeFailureBody is the fixed 500 envelope served when the handler's value
// cannot be marshalled, so the failure is visible to the dashboard instead of
// an implicit 200 with an empty body.
var encodeFailureBody = []byte("{\"error\":\"response encoding failed\"}\n")

// WriteJSONRaw writes a pre-serialised JSON body with the same headers as
// WriteJSON, skipping the encoder (e.g. relaying a remote node's response
// verbatim). An empty body is coerced to `null` so the response is valid JSON.
func WriteJSONRaw(w http.ResponseWriter, raw []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if len(raw) == 0 {
		raw = []byte("null")
	}
	if _, err := w.Write(raw); err != nil {
		slog.Debug("write json raw response", "err", err)
	}
}

// jsonOKBody is the pre-marshaled `{"status":"ok"}` acknowledgement body
// (trailing `\n` matches the encoder's framing).
var jsonOKBody = []byte("{\"status\":\"ok\"}\n")

// WriteOK writes the pre-marshaled `{"status":"ok"}` body with the same headers
// as WriteJSON; prefer it for fixed ack replies.
func WriteOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(jsonOKBody); err != nil {
		slog.Debug("write json response", "err", err)
	}
}

// WriteJSONStatus is like WriteJSON but writes an explicit HTTP status code.
// The body is encoded into the pooled buffer BEFORE WriteHeader so an encode
// failure can still downgrade the status to 500 with an error envelope; the
// header order (Content-Type → WriteHeader → body) is preserved.
func WriteJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	e := getJSONEnc()
	defer putJSONEnc(e)
	var body []byte
	if err := e.enc.Encode(v); err != nil {
		slog.Warn("write json response: encode failed", "status", status, "err", err)
		status = http.StatusInternalServerError
		body = encodeFailureBody
	} else {
		body = e.buf.Bytes()
	}
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
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
// oversize client cannot force unbounded io.ReadAll. Empty body returns
// ErrEmptyJSONBody; MaxBytesError surfaces unchanged for callers that want a
// 413. DisallowUnknownFields is set so a sensitive struct field cannot be
// blind-POSTed before the dashboard exposes it (#1329).
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
// fresh []byte that is safe to outlive the pool. Used on hot paths (WS event
// fanout, session_state broadcasts) instead of json.Marshal.
//
// HTML escaping is disabled (see jsonEncPool), so the WriteJSON CLIENT-SIDE
// CONTRACT applies; a consumer that renders the payload via innerHTML must
// switch to MarshalEscaped (#821).
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

// MarshalEscaped is the HTML-safe counterpart to MarshalPooled for payloads
// spliced into HTML templates, <script type="application/json">, or any
// non-DOMPurify innerHTML path (#821). Intentionally a fresh encoder per call:
// call sites are rare and off the hot path, and a second pool would invite
// the same configuration-mutation hazard as jsonEncPool.
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
