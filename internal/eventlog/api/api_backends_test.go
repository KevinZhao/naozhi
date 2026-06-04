// Package api_test holds the compile-time shadow-regrowth gate for #737
// (RFC eventlog-subsystem-unify Phase 1). The existing in-package
// api_test.go anchors the contract against *cli.EventLog plus a hand-written
// stubReader; this external test pins the assertions to the REAL durable
// backends so a future signature drift in any tier breaks the build instead
// of silently re-growing the three-tier shadow (#1369).
//
// It lives in package api_test (external) so it may import the read-side
// backends (history/naozhilog, history/merged) without widening the
// production api package's import set. None of these backends import
// eventlog/api, so the test edges are cycle-free.
//
// Why only three of the four tiers appear here:
//
//   - cli.EventLog (in-memory ring)  -> Appender + Subscriber  [asserted]
//   - naozhilog.Source (replay)      -> Reader                 [asserted]
//   - merged.Source (composed read)  -> Reader                 [asserted]
//   - persist.Persister (durable spool) -> NONE of the api interfaces.
//
// persist.Persister is fed through a per-key PersistSink callback
// (SinkFor(key) PersistSink) and read back via the package-level
// persist.Recover; it has neither Append(EventEntry)/AppendBatch,
// SubscribeNew, nor LoadBefore. Asserting it against api.Appender/Reader
// today would require a non-thin adapter that wraps the sink model, which
// is out of scope for the behaviour-free Phase 1 (it changes more than a
// zero-logic shim). The gap is intentionally documented here rather than
// papered over so the registry-injection migration (#1570) can decide the
// adapter shape later.
package api_test

import (
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/eventlog/api"
	"github.com/naozhi/naozhi/internal/history/merged"
	"github.com/naozhi/naozhi/internal/history/naozhilog"
)

// Compile-time gate: the canonical in-memory ring backend satisfies the
// write + subscribe halves of the unified contract with no shim.
var (
	_ api.Appender   = (*cli.EventLog)(nil)
	_ api.Subscriber = (*cli.EventLog)(nil)
)

// Compile-time gate: the durable replay reader satisfies api.Reader
// (= cli.HistorySource) exactly, so its results concatenate with the
// ring's without an ordering adapter.
var _ api.Reader = (*naozhilog.Source)(nil)

// Compile-time gate: the merged (local -> fallback) read tier also
// satisfies api.Reader, keeping the composed read path on the same
// contract as its leaves.
var _ api.Reader = (*merged.Source)(nil)
