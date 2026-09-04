// Package limits centralizes cross-package size and count caps so unrelated
// packages don't form reverse dependencies just to share a constant. Add a cap
// here only when at least two packages need it; a single-package constant
// belongs in that package.
package limits

// MaxCoalescedText is a *soft* cap on the merged-prompt size produced by
// dispatch.CoalesceMessages. Worst-case output is cap + per-message ingress
// cap + framing (~5 MB), safely under the shim's 12 MB stdin line ceiling.
// Reverse-RPC handlers and IM ingress reject oversized payloads against this
// same value so the trust boundary holds at every entry point. Kept a const
// so the cap cannot be mutated at run time.
const MaxCoalescedText = 4 * 1024 * 1024

// MaxStreamJSONLine is the cap on a single claude stream-json / tool-result
// line (16 MiB, the CLI's own stdout line ceiling). Every transport carrying
// those lines — node ReverseConn, upstream connector frames, agentevents
// persisted tool results, agentcore SSE (plus its own envelope headroom) —
// bounds a frame/read/file at this value so it cannot drift (#2084).
const MaxStreamJSONLine = 16 << 20

// PlatformReplyMaxAttempts is the retry count for platform.ReplyWithRetry on
// every outbound IM reply path (dispatch and cron), shared so call sites cannot
// drift. 3 attempts fits transient 5xx clearing in 1-2 retries; bumps must keep
// 15s × attempts inside outer ctx deadlines.
const PlatformReplyMaxAttempts = 3
