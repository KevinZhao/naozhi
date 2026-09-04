package httputil

import "net/http"

// WithMaxBytes wraps r.Body with http.MaxBytesReader capped at n bytes and
// returns r for call-site chaining. Every mutating handler must call it with
// MaxRequestBodyBytes (or a tighter cap) before reading the body (#1325).
func WithMaxBytes(w http.ResponseWriter, r *http.Request, n int64) *http.Request {
	r.Body = http.MaxBytesReader(w, r.Body, n)
	return r
}

// MaxMultipartFields caps non-file form entries accepted in a single
// multipart upload.
const MaxMultipartFields = 32

// RejectIfTooManyFields writes a 400 JSON error and returns true when the
// already-parsed multipart form carries more than MaxMultipartFields
// non-file entries. Callers must invoke it immediately after
// ParseMultipartForm and bail out on a true return. A nil MultipartForm
// (non-multipart request) passes.
func RejectIfTooManyFields(w http.ResponseWriter, r *http.Request) bool {
	if r.MultipartForm == nil {
		return false
	}
	total := 0
	for _, vs := range r.MultipartForm.Value {
		total += len(vs)
		if total > MaxMultipartFields {
			WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "too many form fields"})
			return true
		}
	}
	return false
}
