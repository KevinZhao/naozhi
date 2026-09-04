// Package discovery locates and parses Claude CLI on-disk artifacts that
// naozhi reads but does not own: ~/.claude/projects/<slug>/<sessionId>.jsonl
// transcripts, the Claude process tree (for shim attach detection) and the
// per-workspace JSONL index. naozhi's own state lives in internal/eventlog/persist.
//
// Three loosely-coupled sub-domains share the package (#741 tracks a split):
//
//   - Path utilities — scanner.go (ClaudeProjectSlug, projDirName): pure
//     functions over Claude's CWD-derived directory naming.
//   - Process inspection — proc_*.go: platform-specific, read-only /proc and
//     ps(1) helpers that identify live Claude CLI processes by PID so the
//     shim can decide whether to attach to an existing session or spawn one.
//   - History loading — history.go, history_tail.go, recent.go,
//     retired_store.go and the LookupSummaries / extractLastPrompt half of
//     scanner.go: parses Claude's JSONL transcripts into clievent.EventEntry,
//     builds the recent-sessions view and tail-reads active sessions.
//
// Scanner is the per-instance cache holder shared by all three; production
// callers go through DefaultScanner(), tests construct their own via
// NewScanner(). New helpers belong alongside their sub-domain peers, not in
// the nearest open file.
package discovery
