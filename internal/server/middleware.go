package server

import "net/http"

// withMaxBytes wraps r.Body with http.MaxBytesReader at the given limit and
// returns the modified request. Every JSON / multipart entry point must call
// it before any decoder reads r.Body, else an authenticated client can stream
// unbounded garbage into json.NewDecoder (#783).
func withMaxBytes(w http.ResponseWriter, r *http.Request, n int64) *http.Request {
	r.Body = http.MaxBytesReader(w, r.Body, n)
	return r
}
