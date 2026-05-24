// Package wireup centralizes side-effect imports that previously lived
// inside library packages (notably internal/session). Importing wireup
// from the binary entry point (cmd/naozhi) triggers each backend
// package's init() so cli.RegisterHistoryFactory is populated before
// the router is constructed, without forcing internal/session to know
// the concrete backend list.
//
// Sprint 1b of the ARCH-B refactor: pull the blank-imports out of
// internal/session/router_core.go so the session package becomes
// backend-agnostic at the import graph level. Adding a new backend
// only requires adding a blank-import here, not editing session/.
//
// This package contains no exported symbols — its sole purpose is
// to be imported (named or blank) from main; the linker keeps the
// init() side effects regardless.
package wireup

import (
	// Side-effect imports: each backend's init() registers its
	// history.Source factory with cli.RegisterHistoryFactory.
	// Order is irrelevant — RegisterHistoryFactory is idempotent
	// per backend ID and panics on duplicate registration, which
	// surfaces accidental double-wireup at startup rather than at
	// runtime.
	_ "github.com/naozhi/naozhi/internal/history/claudejsonl"
	_ "github.com/naozhi/naozhi/internal/history/kirojsonl"
)
