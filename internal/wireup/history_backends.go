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

// WireHistoryThumbnails installs discovery.ThumbnailFn, which turns raw image
// bytes from a rehydrated JSONL transcript into a small JPEG data URI. The hook
// is a process-global with no build-time enforcement: leave it nil and image
// blocks in history are silently dropped. main calls it once at boot.
func WireHistoryThumbnails() {
	discovery.ThumbnailFn = cli.MakeThumbnail
}
