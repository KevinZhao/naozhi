// Package wireup centralizes boot-time wiring: side-effect imports that
// populate cli.RegisterHistoryFactory, the explicit backend profile
// registration, and cron + sysession construction. Importing it from
// cmd/naozhi keeps internal/session backend-agnostic; adding a backend only
// requires a blank-import here.
package wireup

import (
	// Each backend's init() registers its history.Source factory. Order is
	// irrelevant — RegisterHistoryFactory is last-write-wins per backend ID
	// (duplicate IDs are NOT caught at startup).
	_ "github.com/naozhi/naozhi/internal/history/claudejsonl"
	_ "github.com/naozhi/naozhi/internal/history/codexjsonl"
	_ "github.com/naozhi/naozhi/internal/history/kirojsonl"
)
