// History wiring for cli.Wrapper: the surface the session router uses to ask a
// Wrapper for a HistorySource without cli knowing which backend
// (claudejsonl, kirojsonl, codexjsonl, ...) implements it.
//
// A registry rather than direct imports because internal/history and the
// backends import cli (for HistorySource / RegisterHistoryFactory), so cli
// importing them would cycle; init()-based registration means importing a
// backend package (via internal/wireup) is what binds its factory. cli.HistorySource
// is the canonical contract and history.Source is a type alias for it, so a
// method change is a compile error in every backend (#761).
package cli

import (
	"context"
	"log/slog"
	"sync"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// HistorySessionView is the minimum surface a *cli.Wrapper needs to construct
// a history source for a session; an interface so cli does not import
// internal/session (which imports cli). session.ManagedSession satisfies it.
// SnapshotChainIDs (prevSessionIDs + current, oldest→newest) is re-read on
// every LoadBefore so a /new or workspace switch mid-pagination is observed.
type HistorySessionView interface {
	SessionKey() string
	Workspace() string
	SessionID() string
	SnapshotChainIDs() []string
}

// HistoryWiring carries the directory configuration a HistoryFactoryFn needs.
// The session router fills it from RouterConfig so factories stay pure. All
// fields are optional; a factory with a missing directory should return a
// noop source rather than nil (Wrapper.NewHistorySource enforces non-nil).
type HistoryWiring struct {
	// ClaudeDir is the Claude CLI's projects/ root (~/.claude).
	ClaudeDir string
	// KiroSessionsDir is ~/.kiro/sessions/cli (wired from cmd/naozhi/main.go).
	KiroSessionsDir string
	// CodexSessionsDir is ~/.codex/sessions; the codexjsonl factory globs
	// YYYY/MM/DD/rollout-*-<threadId>.jsonl beneath it.
	CodexSessionsDir string
	// EventLogDir is naozhi's per-session event log directory. Unused by the
	// current factories (naozhilog is wired separately by the router).
	EventLogDir string
}

// HistorySource is the read-only history view returned by a wrapper's
// factory. internal/history.Source is an alias of this type.
type HistorySource interface {
	LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error)
}

// NoopHistorySource is the always-empty HistorySource used when a backend has
// no fallback or HistoryWiring lacks the directory it needs, so callers never
// see nil.
type NoopHistorySource struct{}

// LoadBefore always returns (nil, nil), i.e. "no history available".
func (NoopHistorySource) LoadBefore(context.Context, int64, int) ([]clievent.EventEntry, error) {
	return nil, nil
}

// HistoryFactoryFn produces a HistorySource for a session against a given
// wiring snapshot. Returning nil is allowed; Wrapper.NewHistorySource upgrades
// it to NoopHistorySource{}.
type HistoryFactoryFn func(s HistorySessionView, deps HistoryWiring) HistorySource

// historyFactoryRegistry maps backend ID → factory, populated from backend
// init() blocks. Mutex-guarded because tests register replacement factories
// from t.Run blocks.
var (
	historyFactoryMu       sync.RWMutex
	historyFactoryRegistry = map[string]HistoryFactoryFn{}

	// missingFactoryWarned dedups the "no history factory" Warn to one line
	// per backend ID, since the lookup runs on every history page (#975).
	missingFactoryMu     sync.Mutex
	missingFactoryWarned = map[string]bool{}
)

// RegisterHistoryFactory binds a backend ID to its history-source factory,
// intended for a backend package's init(). backendID "" is ignored (it means
// "router default" and never reaches a wrapper). Re-registering overwrites;
// tests rely on this to inject failing factories.
func RegisterHistoryFactory(backendID string, fn HistoryFactoryFn) {
	if backendID == "" || fn == nil {
		return
	}
	historyFactoryMu.Lock()
	defer historyFactoryMu.Unlock()
	historyFactoryRegistry[backendID] = fn
}

// warnMissingHistoryFactory logs a one-time Warn for a non-empty backend ID
// with no registered factory (likely a missing wireup blank-import) (#975).
func warnMissingHistoryFactory(backendID string) {
	if backendID == "" {
		return
	}
	missingFactoryMu.Lock()
	already := missingFactoryWarned[backendID]
	if !already {
		missingFactoryWarned[backendID] = true
	}
	missingFactoryMu.Unlock()
	if already {
		return
	}
	slog.Warn("cli: no history factory registered for backend; history will be empty",
		"backend", backendID,
		"hint", "ensure the backend's history package is blank-imported via internal/wireup")
}

// pickHistoryFactory looks up the factory for a backend ID; nil when none is
// registered.
func pickHistoryFactory(backendID string) HistoryFactoryFn {
	if backendID == "" {
		return nil
	}
	historyFactoryMu.RLock()
	defer historyFactoryMu.RUnlock()
	return historyFactoryRegistry[backendID]
}

// NewHistorySource constructs a HistorySource for the supplied session using
// the factory currently registered for the wrapper's BackendID. Always
// non-nil: a nil receiver, an unregistered backend, or a nil-returning factory
// all degrade to NoopHistorySource.
//
// The registry is consulted on every call, not cached at NewWrapper time, so
// a RegisterHistoryFactory that lands after construction (per-t.Run tests,
// lazily imported backends) is honoured; the RWMutex read is ~30ns.
func (w *Wrapper) NewHistorySource(s HistorySessionView, deps HistoryWiring) HistorySource {
	if w == nil {
		return NoopHistorySource{}
	}
	fn := pickHistoryFactory(w.BackendID)
	if fn == nil {
		// Non-empty BackendID with no factory = missing wireup import; warn
		// once so "history is empty" has an operator-visible cause (#975).
		warnMissingHistoryFactory(w.BackendID)
		return NoopHistorySource{}
	}
	src := fn(s, deps)
	if src == nil {
		return NoopHistorySource{}
	}
	return src
}
