// Package tracker observes event-log persist batches and maintains
// the reference-count metadata on every attachment file they touch.
// See docs/rfc/attachment-refcount.md for the full design; this
// package implements the middle layer between:
//
//   - internal/session (produces (keyhash, imagePaths[], timeMS)
//     tuples whenever an EventEntry with ImagePaths reaches the
//     persist sink without replayPhase)
//   - internal/attachment (stores the canonical Meta struct on
//     disk; its Meta.AddReference / UpdateMetaFile helpers do the
//     read-modify-write.)
//
// Concurrency model:
//
//   - Exactly one worker goroutine owns every .meta write. Events
//     arrive via a buffered channel; the worker coalesces repeated
//     bumps within a short debounce window before writing, so a
//     heavy image-laden session does not turn into a meta-write
//     storm.
//   - Callers (the session-layer sink bridge, Router.Remove) are
//     never blocked: a full channel drops the event with a metric
//     increment, identical policy to the event-log Persister.
//   - Reads (dashboard handler, GCWithRefs) do not coordinate with
//     the worker — Meta files are written atomically (tmp + rename)
//     so readers either see the pre- or post-write snapshot.
//
// The tracker is OPT-IN: callers pass a nil workspace resolver or
// never call NewTracker when event-log persistence is disabled.
// The session layer's wire-up in Router.NewRouter guards the
// lifecycle.
package tracker
