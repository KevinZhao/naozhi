// Package discovery locates and parses Claude CLI on-disk artifacts that
// naozhi needs to read but does NOT own: ~/.claude/projects/<slug>/<sessionId>.jsonl
// transcripts, the Claude process tree (for shim attach detection), and
// the per-workspace JSONL index. It is the "look at what Claude wrote and
// what Claude is doing" half of naozhi; the "write naozhi's own state"
// half is internal/eventlog/persist.
//
// # Sub-domains (R222-ARCH-16)
//
// Three loosely-coupled responsibilities currently share this directory.
// A 2026-04 architecture review (R222-ARCH-16, #741) flagged the mixing
// and proposed splitting into discovery/path + discovery/proc +
// history/claudejsonl. Until that lands, treat this godoc as the
// topographical map; the in-domain extraction is constrained by the
// cli-package import graph (cli.EventEntry is produced here in
// LoadHistory/parseJSONL) and is best done in lockstep with the
// R237-ARCH-13 eventlog rename so the history-source boundary moves only
// once. Sub-domains today:
//
//   - Path utilities — scanner.go (ClaudeProjectSlug, projDirName),
//     workspace_jsonl.go: pure functions over Claude's CWD-derived
//     directory naming. No I/O on the hot path beyond a single Stat.
//     Future home: discovery/path. Stable, no behaviour change planned.
//
//   - Process inspection — proc_*.go (proc_linux.go,
//     proc_darwin.go, proc_windows.go, proc_errors.go,
//     proc_backend_profile_test.go): platform-specific helpers that
//     enumerate live Claude CLI processes by PID/exe so the shim can
//     decide whether to attach to an existing session vs spawn a new
//     one. Pure read-only /proc + sysctl reads; no Claude file
//     parsing. Future home: discovery/proc.
//
//   - History loading — history.go, history_tail.go, recent.go,
//     scanner.go (the LookupSummaries / extractLastPrompt half),
//     retired_store.go, safe_json_test.go: parses Claude's JSONL
//     transcripts into cli.EventEntry, builds the recent-sessions
//     view, and tail-watches active sessions. The semantically
//     largest sub-domain (~3.5 kLoC). Future home:
//     internal/history/claudejsonl, merging with the existing
//     thinly-populated package of the same name.
//
// # Cross-cutting types
//
//   - Scanner — per-instance cache holder (promptCache,
//     summaryCache, pathCache). Replaces the package-level globals
//     so tests can run in parallel; production callers go through
//     DefaultScanner(). Survives any future split because each
//     extracted package will own its own cache slice.
//
//   - claude_project_slug_*: Claude's project-directory naming
//     convention — `/Users/foo/bar/baz` → `-Users-foo-bar-baz` (with
//     control-byte sanitisation). Used by both the path utilities
//     and the history loader; will end up in discovery/path with the
//     latter calling into it.
//
// # Why the split is deferred
//
// Splitting requires either:
//
//  1. Moving cli.EventEntry into a non-cli package so the new
//     history/claudejsonl can import it without pulling cli's
//     process-management surface, OR
//  2. Defining a discovery-level event type and bridging at the
//     consumer (router) — duplicating the schema until the cli
//     package itself splits (R230B-ARCH-11).
//
// Both options entangle with R237-ARCH-13 (eventlog ring/persist/replay
// rename) and R226-ARCH-14 (cli payload extraction). Doing the
// extraction now would either force one of those bigger refactors
// inline or leave a partial-bridge that gets undone when they land.
// The pragmatic stance is: keep the boundaries documented (this file)
// and the godoc cross-references on Scanner / LoadHistory / proc
// helpers explicit, so when the upstream refactors land the discovery
// split becomes a mostly-mechanical move.
//
// Reviewers MUST treat the three sub-domains as semantically separate
// even though they share a package name. Adding a new helper goes
// alongside its sub-domain peers (path/proc/history.go), not in the
// nearest open file.
package discovery
