// contract.go — the history-source contract and its backend registry
// (#2649 G1-d).
//
// # The inversion this fixes
//
// These declarations lived in internal/cli, and internal/cli/history.go said so
// out loud: "A registry rather than direct imports because internal/history and
// the backends import cli (for HistorySource / RegisterHistoryFactory), so cli
// importing them would cycle". The disk readers imported the process manager to
// learn what shape to be, and internal/history could only offer
// `type Source = cli.HistorySource` — an alias pointing the wrong way.
//
// The dependency is now the way round it should be: this package owns the
// contract, the backends implement it, and internal/cli imports it to look a
// factory up. cli.Wrapper still resolves the factory, because the wrapper is
// what knows the backend ID.
//
// The init()-based registry stays. It is not a workaround for the cycle — it is
// how a backend becomes available by being linked (internal/wireup blank-imports
// them), so naozhi builds without a backend do not carry its file-format code.
package history

import (
	"context"
	"log/slog"
	"sync"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// SessionView is the minimum surface a factory needs to construct a history
// source for a session; an interface so neither this package nor internal/cli
// has to import internal/session. session.ManagedSession satisfies it.
// SnapshotChainIDs (prevSessionIDs + current, oldest→newest) is re-read on
// every LoadBefore so a /new or workspace switch mid-pagination is observed.
type SessionView interface {
	SessionKey() string
	Workspace() string
	SessionID() string
	SnapshotChainIDs() []string
}

// Wiring carries the directory configuration a FactoryFn needs. The session
// router fills it from RouterConfig so factories stay pure. All fields are
// optional; a factory with a missing directory should return a noop source
// rather than nil (Wrapper.NewHistorySource enforces non-nil).
type Wiring struct {
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

// Source exposes a read-only view of a session's past events.
//
// Implementations must be safe for concurrent use. LoadBefore returns up to
// `limit` entries with Time strictly less than `beforeMS`, oldest → newest,
// mirroring ring.EventLog.EntriesBefore. beforeMS <= 0 means no upper bound;
// limit <= 0 returns nil. Errors are informational: callers log and treat them
// as end-of-history. ctx cancellation must propagate into file I/O.
type Source interface {
	LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error)
}

// Noop is the always-empty Source used when a backend has no durable history or
// Wiring lacks the directory it needs, so callers never see nil.
type Noop struct{}

// LoadBefore always returns (nil, nil), i.e. "no history available".
func (Noop) LoadBefore(context.Context, int64, int) ([]clievent.EventEntry, error) {
	return nil, nil
}

// FactoryFn produces a Source for a session against a given wiring snapshot.
// Returning nil is allowed; Wrapper.NewHistorySource upgrades it to Noop{}.
type FactoryFn func(s SessionView, deps Wiring) Source

// factoryRegistry maps backend ID → factory, populated from backend init()
// blocks. Mutex-guarded because tests register replacement factories from
// t.Run blocks.
var (
	factoryMu       sync.RWMutex
	factoryRegistry = map[string]FactoryFn{}

	// missingFactoryWarned dedups the "no history factory" Warn to one line per
	// backend ID, since the lookup runs on every history page (#975).
	missingFactoryMu     sync.Mutex
	missingFactoryWarned = map[string]bool{}
)

// RegisterFactory binds a backend ID to its history-source factory, intended
// for a backend package's init(). backendID "" is ignored (it means "router
// default" and never reaches a wrapper). Re-registering overwrites; tests rely
// on this to inject failing factories.
func RegisterFactory(backendID string, fn FactoryFn) {
	if backendID == "" || fn == nil {
		return
	}
	factoryMu.Lock()
	defer factoryMu.Unlock()
	factoryRegistry[backendID] = fn
}

// PickFactory looks up the factory for a backend ID; nil when none is
// registered. Exported because the resolver lives in internal/cli: the wrapper
// is what knows its backend ID.
func PickFactory(backendID string) FactoryFn {
	if backendID == "" {
		return nil
	}
	factoryMu.RLock()
	defer factoryMu.RUnlock()
	return factoryRegistry[backendID]
}

// WarnMissingFactory logs a one-time Warn for a non-empty backend ID with no
// registered factory (likely a missing wireup blank-import) (#975). Exported
// alongside PickFactory for the same reason.
func WarnMissingFactory(backendID string) {
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
	// Message text unchanged from when this lived in internal/cli: it is an
	// operator-facing string that log searches and one test match on. Only the
	// package prefix moved.
	slog.Warn("history: no history factory registered for backend; history will be empty",
		"backend", backendID,
		"hint", "ensure the backend's history package is blank-imported via internal/wireup")
}
