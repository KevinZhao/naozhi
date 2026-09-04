package cron

import (
	"bytes"
	"encoding/json"
	"strings"
)

// redactedJSONPlaceholder replaces a JSON value that could not be redacted
// while keeping it well-formed, so the enclosing response stays encodable
// instead of json.Encoder failing on the whole payload.
var redactedJSONPlaceholder = json.RawMessage(`{"redacted":true}`)

// redactRawJSON applies fn (a text redactor such as textutil.RedactSecrets)
// to a raw JSON document without breaking its structure. Fast path: fn runs
// over the raw text and the result is returned if it is still valid JSON
// (redaction markers are legal inside JSON strings). Slow path: when the
// substitution damaged the structure, the document is decoded, fn is applied
// to every string value, and the tree is re-encoded (json.Number preserves
// numeric precision). Undecodable input yields redactedJSONPlaceholder.
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
