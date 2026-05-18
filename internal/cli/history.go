// History wiring for cli.Wrapper. This file declares the small surface the
// session router needs to ask a Wrapper "give me a history.Source for this
// session" without the cli package having to know which concrete backend
// (claudejsonl, kirojsonl, ...) implements it.
//
// Why a registry instead of direct imports:
//
//   - internal/history and internal/history/claudejsonl both import
//     internal/cli (for cli.EventEntry). If cli imported either of them
//     the build would cycle. The history package intentionally lives one
//     level "above" cli in the dependency graph.
//   - We want backend-specific factories (claudeHistoryFactory and the
//     future kiroHistoryFactory) to be wired in their own packages so a
//     new backend lands as a single new file, not a session-package edit.
//   - init()-based registration keeps the binding side-effect explicit:
//     any package that imports a history backend (e.g. session importing
//     internal/history/claudejsonl) automatically gets its factory
//     registered with cli.
//
// The HistorySource interface declared here is structurally identical to
// internal/history.Source (same single LoadBefore method). Go interface
// satisfaction is structural, so any history.Source value satisfies
// cli.HistorySource without an explicit adapter — callers can return a
// claudejsonl.Source straight from a HistoryFactoryFn even though the
// concrete type's compile-time interface is history.Source.
package cli

import (
	"context"
	"sync"
)

// HistorySessionView is the minimum surface a *cli.Wrapper needs to
// construct a history source for a session. Defined as an interface so
// the cli package does not have to import internal/session (which would
// cycle: session already imports cli).
//
// session.ManagedSession satisfies this interface today through:
//   - SessionKey() — returns the immutable session key
//   - Workspace() — returns the effective cwd
//   - SessionID()  — returns the current CLI session ID
//   - SnapshotChainIDs() — returns prevSessionIDs + current ID, oldest→newest
//
// SnapshotChainIDs in particular is invoked by claudejsonl's chain reader
// on every LoadBefore call so a /new or workspace switch mid-pagination
// is observed by the next page.
type HistorySessionView interface {
	SessionKey() string
	Workspace() string
	SessionID() string
	SnapshotChainIDs() []string
}

// HistoryWiring carries the directory configuration a HistoryFactoryFn
// needs to construct a backend-specific source. The session router
// populates this from RouterConfig at attachHistorySource time so the
// factory itself stays pure (no router-internal references leak into
// cli's public surface).
//
// All fields are optional. An empty value typically means "this backend
// has no fallback source"; the factory is expected to return a noop
// source rather than nil. Wrapper.NewHistorySource enforces non-nil at
// the boundary regardless.
type HistoryWiring struct {
	// ClaudeDir is the Claude CLI's projects/ root (~/.claude). The
	// claude factory reads per-session JSONL files from beneath this
	// directory.
	ClaudeDir string
	// KiroSessionsDir is ~/.kiro/sessions/cli, used by the future
	// kirojsonl source. Sprint 1a leaves this unwired; Sprint 1b will
	// populate it from cmd/naozhi/main.go.
	KiroSessionsDir string
	// EventLogDir is naozhi's per-session event log directory. Listed
	// here for symmetry; current backend factories don't read it
	// (naozhilog is the local tier in MergedSource and is wired
	// separately by the session router) but a future backend that
	// needed cross-session state could.
	EventLogDir string
}

// HistorySource is the read-only history view returned by a wrapper's
// factory. Structurally identical to internal/history.Source —
// implementations of one satisfy the other automatically.
type HistorySource interface {
	LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]EventEntry, error)
}

// NoopHistorySource is the always-empty HistorySource used when a backend
// has no fallback or HistoryWiring is missing the directory it needs.
// Callers always get a non-nil HistorySource so they can skip nil checks.
type NoopHistorySource struct{}

// LoadBefore always returns (nil, nil). Callers interpret this as "no
// history available" on the first call.
func (NoopHistorySource) LoadBefore(context.Context, int64, int) ([]EventEntry, error) {
	return nil, nil
}

// HistoryFactoryFn produces a HistorySource for a session against a given
// wiring snapshot. Returning nil is allowed; Wrapper.NewHistorySource
// upgrades nil to NoopHistorySource{} so callers always have a valid
// source to call LoadBefore on.
type HistoryFactoryFn func(s HistorySessionView, deps HistoryWiring) HistorySource

// historyFactoryRegistry maps backend ID → factory. Populated via
// RegisterHistoryFactory from history backend init() blocks. Read-only
// after process startup but guarded by a mutex anyway because tests can
// register replacement factories from t.Run blocks.
var (
	historyFactoryMu       sync.RWMutex
	historyFactoryRegistry = map[string]HistoryFactoryFn{}
)

// RegisterHistoryFactory binds a backend ID to its history-source
// factory. Intended to be called from a backend package's init() so the
// binding happens whenever that package is imported. backendID "" is
// silently ignored — the empty backend ID means "router default" at
// session-construction time and never reaches a wrapper.
//
// Re-registering a backend ID overwrites the previous factory; the last
// registration wins. Tests rely on this to inject failing factories.
func RegisterHistoryFactory(backendID string, fn HistoryFactoryFn) {
	if backendID == "" || fn == nil {
		return
	}
	historyFactoryMu.Lock()
	defer historyFactoryMu.Unlock()
	historyFactoryRegistry[backendID] = fn
}

// pickHistoryFactory looks up the factory for a backend ID. Returns nil
// when no factory is registered — Wrapper.NewHistorySource maps that to
// NoopHistorySource so missing registrations never produce panics.
func pickHistoryFactory(backendID string) HistoryFactoryFn {
	if backendID == "" {
		return nil
	}
	historyFactoryMu.RLock()
	defer historyFactoryMu.RUnlock()
	return historyFactoryRegistry[backendID]
}

// NewHistorySource constructs a HistorySource for the supplied session
// using the wrapper's bound backend factory. Always returns a non-nil
// source: a nil receiver, an unregistered backend, or a factory that
// returns nil all degrade to NoopHistorySource so call sites can treat
// the return value as never-nil.
func (w *Wrapper) NewHistorySource(s HistorySessionView, deps HistoryWiring) HistorySource {
	if w == nil || w.historyFactory == nil {
		return NoopHistorySource{}
	}
	src := w.historyFactory(s, deps)
	if src == nil {
		return NoopHistorySource{}
	}
	return src
}
