package shim

// Per-line byte limits for the shim socket protocol.

// maxClientLineBytes is the per-line read limit for client frames.
const maxClientLineBytes = 16 * 1024 * 1024 // 16MB

// maxWriteLineBytes caps the inner "line" field of a post-auth "write" frame
// before it is piped into CLI stdin. Every byte of msg.Line flows through
// bufio.Scanner on the Claude side (10 MB default buffer); matching the
// naozhi-side producer limit (cli.maxStdinLineBytes = 12 MB) keeps the shim a
// faithful pass-through while refusing anything that would overflow Claude's
// scanner and silently kill its stdout.
const maxWriteLineBytes int64 = 12 * 1024 * 1024 // 12MB
