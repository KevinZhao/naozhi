// Package limits centralizes cross-package size and count caps so unrelated
// packages don't form one-way reverse dependencies just to share a single
// constant. Existing examples:
//
//   - MaxCoalescedTextBytes — historically lived in internal/dispatch and
//     was reached into by internal/upstream's reverse-RPC handler purely to
//     reuse the cap value (R228-ARCH-9). The two packages have nothing else
//     in common, so the cleaner home is a leaf utility package both can
//     import.
//
// Add new caps here only when at least two packages need to share them; a
// constant used by a single package belongs in that package.
package limits

// MaxCoalescedText is a *soft* cap on the merged-prompt size produced by
// dispatch.CoalesceMessages. Worst-case output:
//
//	cap + per-message-ingress-cap + small framing overhead
//
// which is ~5 MB for the current 4 MB cap and 1 MB per-message ingress cap.
// Safely under the shim's 12 MB stdin line ceiling.
//
// Reverse-RPC handlers (e.g. upstream's `send` case) and IM ingress paths
// reject oversized payloads against this same value before they reach
// CoalesceMessages so the trust boundary is enforced at every entry point.
//
// Keep as a const, not a var: the cap is deliberately compile-time stable
// to prevent accidental run-time mutation by tests or config-loading code.
const MaxCoalescedText = 4 * 1024 * 1024
