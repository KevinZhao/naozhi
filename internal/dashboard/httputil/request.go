package httputil

import "net/http"

// WithMaxBytes wraps r.Body with http.MaxBytesReader capped at n bytes and
// returns r for call-site chaining. R20260527122801-SEC-1 (#1325) is
// enforced at every mutating handler by passing MaxRequestBodyBytes (or a
// tighter per-endpoint cap) here before the body is read. Previously
// duplicated per sub-package (#2285); internal/server keeps its own private
// copy until its middleware moves.
func WithMaxBytes(w http.ResponseWriter, r *http.Request, n int64) *http.Request {
	r.Body = http.MaxBytesReader(w, r.Body, n)
	return r
}

// MaxMultipartFields caps non-file form entries accepted in a single
// multipart upload. Dashboard uploads carry a handful of known fields; 32
// leaves slack for growth without permitting unbounded form-value maps.
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
