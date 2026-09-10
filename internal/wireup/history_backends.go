// Package wireup centralizes boot-time wiring: side-effect imports that
// populate history.RegisterFactory, the explicit backend profile
// registration, and cron + sysession construction. Importing it from
// cmd/naozhi keeps internal/session backend-agnostic; adding a backend only
// requires a blank-import here.
package wireup

import (
	// Each backend's init() registers its history.Source factory. Order is
	// irrelevant — history.RegisterFactory is last-write-wins per backend ID
	// (duplicate IDs are NOT caught at startup).
	_ "github.com/naozhi/naozhi/internal/history/claudejsonl"
	_ "github.com/naozhi/naozhi/internal/history/codexjsonl"
	_ "github.com/naozhi/naozhi/internal/history/kirojsonl"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
)

// discovery.ThumbnailFn turns raw image bytes from a rehydrated JSONL transcript
// into a small JPEG data URI. It is a process-global hook with no build-time
// enforcement: leave it nil and image blocks in history are silently dropped, no
// compile error, no warning.
//
// The assignment used to sit in internal/history/claudejsonl's init(), which was
// that package's last reason to import internal/cli (#2649 G1-d) — a JSONL format
// reader pulling in the subprocess manager for one function pointer. A
// cross-package hook belongs at the wiring site, and this file is already the
// place a backend becomes available by being linked.
//
// history_thumbnail_test.go asserts the hook is non-nil after this package's
// init, since nothing else would notice.
func init() {
	discovery.ThumbnailFn = cli.MakeThumbnail
}
