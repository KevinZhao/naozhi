package cron

import (
	"bytes"
	"encoding/json"
	"strings"
)

// redactedJSONPlaceholder replaces a JSON value that could not be redacted
// while keeping it well-formed (malformed source bytes). Serving a fixed
// placeholder keeps the enclosing response encodable — the alternative was
// json.Encoder failing on the whole payload and the panel showing nothing.
var redactedJSONPlaceholder = json.RawMessage(`{"redacted":true}`)

// redactRawJSON applies fn (a text redactor such as textutil.RedactSecrets)
// to a raw JSON document without breaking its structure.
//
// Fast path: fn is run over the raw text. If nothing changed and the input
// is valid JSON it is returned as-is (aliased). If the substituted text is
// still valid JSON — the overwhelmingly common case, since redaction markers
// are legal inside JSON strings — it is returned directly.
//
// Slow path: when the text-level substitution damaged the structure (a
// marker swallowed a closing quote / escape), the document is decoded, fn is
// applied to every string value individually, and the tree is re-encoded.
// Numbers round-trip via json.Number so precision is preserved. Input that
// cannot be decoded at all yields redactedJSONPlaceholder.
func redactRawJSON(raw string, fn func(string) string) json.RawMessage {
	out := fn(raw)
	if out == raw {
		if json.Valid([]byte(raw)) {
			return json.RawMessage(raw)
		}
		return redactedJSONPlaceholder
	}
	if json.Valid([]byte(out)) {
		return json.RawMessage(out)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return redactedJSONPlaceholder
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(redactJSONStrings(v, fn)); err != nil {
		return redactedJSONPlaceholder
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

// redactJSONStrings walks a decoded JSON tree and applies fn to every string
// value. Object keys are left untouched (they are field names, not
// payload); containers are rebuilt rather than mutated in place.
func redactJSONStrings(v any, fn func(string) string) any {
	switch t := v.(type) {
	case string:
		return fn(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = redactJSONStrings(val, fn)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactJSONStrings(val, fn)
		}
		return out
	default:
		return v
	}
}
