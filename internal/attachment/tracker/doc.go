// Package tracker observes event-log persist batches and maintains the
// reference-count metadata on every attachment file they touch, between
// internal/session (emits (keyhash, imagePaths[], timeMS)) and
// internal/attachment (owns Meta and the read-modify-write helpers).
//
// Concurrency: exactly one worker goroutine owns every .meta write; events
// arrive via a buffered channel and repeated bumps are coalesced within a
// debounce window. Callers never block — a full channel drops the event with
// a metric increment. Readers (dashboard, GCWithRefs) rely on atomic writes.
//
// Opt-in: Router.NewRouter wires it only when event-log persistence is on.
package tracker
