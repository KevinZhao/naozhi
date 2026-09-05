package shim

// Per-connection and per-line byte limits for the shim socket protocol;
// setters exist for tests, values are process-wide atomics.

import "sync/atomic"

// defaultMaxClientLineBytes is the compiled-in per-line size limit.
const defaultMaxClientLineBytes = 16 * 1024 * 1024 // 16MB

// maxClientLineBytesAtomic holds the per-line limit so tests can dial it
// down without racing the runCommandLoop reader; zero means default (#701).

// maxClientLineBytesAtomic holds the per-line limit so tests can dial it
// down without racing the runCommandLoop reader; zero means default (#701).
var maxClientLineBytesAtomic atomic.Int64

// maxClientLineBytes returns the active per-line read limit.

// maxClientLineBytes returns the active per-line read limit.
func maxClientLineBytes() int {
	v := maxClientLineBytesAtomic.Load()
	if v == 0 {
		return defaultMaxClientLineBytes
	}
	return int(v)
}

// setMaxClientLineBytes overrides the per-line limit for tests; returns the
// previous value, zero resets to the default.

// setMaxClientLineBytes overrides the per-line limit for tests; returns the
// previous value, zero resets to the default.
func setMaxClientLineBytes(v int) int {
	return int(maxClientLineBytesAtomic.Swap(int64(v)))
}

// defaultMaxClientSessionBytes is the cumulative post-auth byte budget per
// client connection: the per-line LimitedReader resets every iteration, so
// without it an authenticated client could churn near-16-MB lines
// indefinitely (memory-pressure DoS). Generous on purpose (#541).

// defaultMaxClientSessionBytes is the cumulative post-auth byte budget per
// client connection: the per-line LimitedReader resets every iteration, so
// without it an authenticated client could churn near-16-MB lines
// indefinitely (memory-pressure DoS). Generous on purpose (#541).
const defaultMaxClientSessionBytes int64 = defaultMaxClientLineBytes * 1000

// maxClientSessionBytes is atomic so tests can dial it down without racing
// the recv hot path; zero means default.

// maxClientSessionBytes is atomic so tests can dial it down without racing
// the recv hot path; zero means default.
var maxClientSessionBytes atomic.Int64

// maxClientSessionBytesValue returns the active cumulative cap. A zero
// stored value resolves to defaultMaxClientSessionBytes.

// maxClientSessionBytesValue returns the active cumulative cap. A zero
// stored value resolves to defaultMaxClientSessionBytes.
func maxClientSessionBytesValue() int64 {
	v := maxClientSessionBytes.Load()
	if v == 0 {
		return defaultMaxClientSessionBytes
	}
	return v
}

// setMaxClientSessionBytes overrides the cumulative cap for tests; returns
// the previous value, zero resets to the default.

// setMaxClientSessionBytes overrides the cumulative cap for tests; returns
// the previous value, zero resets to the default.
func setMaxClientSessionBytes(v int64) int64 {
	return maxClientSessionBytes.Swap(v)
}

// defaultMaxWriteLineBytes caps the inner "line" field of a post-auth "write"
// frame before it is piped into CLI stdin. Every byte of msg.Line flows
// through bufio.Scanner on the Claude side (10 MB default buffer); matching
// the naozhi-side producer limit (cli.maxStdinLineBytes = 12 MB) keeps the
// shim a faithful pass-through while refusing anything that would overflow
// Claude's scanner and silently kill its stdout. Held in an atomic so tests
// can dial it down without racing the recv hot path (#701).

// defaultMaxWriteLineBytes caps the inner "line" field of a post-auth "write"
// frame before it is piped into CLI stdin. Every byte of msg.Line flows
// through bufio.Scanner on the Claude side (10 MB default buffer); matching
// the naozhi-side producer limit (cli.maxStdinLineBytes = 12 MB) keeps the
// shim a faithful pass-through while refusing anything that would overflow
// Claude's scanner and silently kill its stdout. Held in an atomic so tests
// can dial it down without racing the recv hot path (#701).
const defaultMaxWriteLineBytes int64 = 12 * 1024 * 1024 // 12MB

var maxWriteLineBytes atomic.Int64

// maxWriteLineBytesValue returns the active write-line cap; zero means default.

// maxWriteLineBytesValue returns the active write-line cap; zero means default.
func maxWriteLineBytesValue() int64 {
	v := maxWriteLineBytes.Load()
	if v == 0 {
		return defaultMaxWriteLineBytes
	}
	return v
}

// setMaxWriteLineBytes overrides the cap for tests; returns the previous
// value, zero resets to the default.

// setMaxWriteLineBytes overrides the cap for tests; returns the previous
// value, zero resets to the default.
func setMaxWriteLineBytes(v int64) int64 {
	return maxWriteLineBytes.Swap(v)
}

// Shim server timers (semantically independent despite equal values):
//   - shimSocketWatchInterval: stat() poll cadence detecting a deleted AF_UNIX
//     socket file so an orphaned shim can self-shutdown.
//   - shimShutdownGracePeriod: window after SIGTERM/SIGINT for a fresh client
//     Attach; otherwise the shim exits.
//   - shimAuthReadDeadline: wait for a connecting peer's first line.
