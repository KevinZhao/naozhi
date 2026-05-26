package server

import "net/http"

// withMaxBytes wraps r.Body with http.MaxBytesReader at the given limit and
// returns the modified request. R246-ARCH-7 (#783): every JSON / multipart
// HTTP entry point in this package needs to bound the inbound body before
// any decoder reads from r.Body — otherwise a malicious client with a valid
// auth token can stream gigabytes of garbage into json.NewDecoder, eating
// memory and goroutine time before the parser bails. The limiter was
// previously written inline at 13+ call sites with hand-typed limits like
// `1<<10`, `64*1024`, `2<<20`; new handlers routinely forgot the wrap and
// the compiler had no way to flag the omission. Routing every JSON entry
// point through this helper:
//
//   - keeps the limit visible at the handler's first line so a code review
//     can confirm the cap matches the schema,
//   - encourages a small set of named constants instead of magic numbers,
//   - lets a future linter assert "every handler that decodes r.Body must
//     have called withMaxBytes (or the request must come from an explicit
//     allowlist like file-upload paths that wrap separately)".
//
// The function intentionally returns *http.Request rather than mutating in
// place: an Edit that swaps the call site needs a single rebind line and
// never accidentally reuses the original r alongside the wrapped body.
func withMaxBytes(w http.ResponseWriter, r *http.Request, n int64) *http.Request {
	r.Body = http.MaxBytesReader(w, r.Body, n)
	return r
}
